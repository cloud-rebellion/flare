// Package native_symbols contains the bounded native debug-artifact boundary.
//
// Flutter's --split-debug-info output is a DWARF container.  Flutter deliberately
// does not promise one stable container format: Android builds are commonly ELF
// and iOS builds can be Mach-O or a dSYM bundle.  This package therefore only
// answers whether an uploaded artifact is a recognised debug container.  It
// never turns an upload into a symbolicated frame and never returns artifact
// bytes to a caller.  Format-specific frame rewriting remains a separate,
// explicitly enabled implementation.
package native_symbols

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
)

// Status is deliberately about what Flare has proved, not what a caller hopes
// an artifact can do.
type Status string

const (
	StatusRetainedUnresolved Status = "retained_unresolved"
	StatusRecognized         Status = "recognized_unresolved"
	StatusUnsupported        Status = "unsupported"
	StatusInvalid            Status = "invalid"
)

// Validation is metadata only.  The input bytes are not retained after the
// call and are never copied into an API response.
type Validation struct {
	Status       Status
	Container    string
	Architecture string
	HasDebugInfo bool
	Reason       string
}

// ValidateDartSymbols recognises the bounded containers currently seen in
// Flutter --split-debug-info artifacts.  It intentionally does not parse DWARF
// line programs: the generated file is not guaranteed to remain a stable
// format, and claiming source locations from a partial parser would make an
// event appear fixed when it was not.
func ValidateDartSymbols(content []byte) Validation {
	if len(content) == 0 {
		return Validation{Status: StatusInvalid, Reason: "artifact is empty"}
	}
	if v, ok := validateELF(content); ok {
		return v
	}
	if v, ok := validateMachO(content); ok {
		return v
	}
	return Validation{
		Status: StatusUnsupported,
		Reason: "container is not a supported ELF or Mach-O debug artifact",
	}
}

func validateELF(content []byte) (Validation, bool) {
	if len(content) < 4 || !bytes.Equal(content[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return Validation{}, false
	}
	f, err := elf.NewFile(bytes.NewReader(content))
	if err != nil {
		return Validation{Status: StatusInvalid, Container: "elf", Reason: "invalid ELF container"}, true
	}
	debugInfo := f.Section(".debug_info") != nil || f.Section(".debug_line") != nil ||
		f.Section(".zdebug_info") != nil || f.Section(".zdebug_line") != nil
	arch := elfArch(f.Machine)
	if !debugInfo {
		return Validation{
			Status: StatusRecognized, Container: "elf", Architecture: arch,
			Reason: "ELF container has no recognised DWARF section; frame rewriting remains unresolved",
		}, true
	}
	return Validation{
		Status: StatusRecognized, Container: "elf", Architecture: arch,
		HasDebugInfo: true,
		Reason:       "DWARF container recognised; frame rewriting remains unresolved",
	}, true
}

func validateMachO(content []byte) (Validation, bool) {
	if len(content) < 4 {
		return Validation{}, false
	}
	magic := binary.BigEndian.Uint32(content[:4])
	// NewFile handles the endian variants; this quick check keeps arbitrary
	// bytes away from the parser and allows us to distinguish unsupported input.
	if magic != 0xfeedface && magic != 0xfeedfacf && magic != 0xcefaedfe && magic != 0xcffaedfe {
		// A fat/universal Mach-O starts with CAFEBABE or CAFEBABF.
		if magic != 0xcafebabe && magic != 0xcafebabf && magic != 0xbebafeca && magic != 0xbfbafeca {
			return Validation{}, false
		}
		fat, err := macho.NewFatFile(bytes.NewReader(content))
		if err != nil {
			return Validation{Status: StatusInvalid, Container: "macho", Reason: "invalid Mach-O universal container"}, true
		}
		defer fat.Close()
		if len(fat.Arches) == 0 {
			return Validation{Status: StatusInvalid, Container: "macho", Reason: "Mach-O universal container has no architectures"}, true
		}
		// Validate at most the first architecture.  The artifact is already
		// bounded by the upload endpoint; this keeps health reads deterministic
		// without multiplying work by an attacker-controlled arch count.
		for _, arch := range fat.Arches[:1] {
			v := validateMachOFile(arch.File)
			v.Container = "macho-fat"
			return v, true
		}
	}
	f, err := macho.NewFile(bytes.NewReader(content))
	if err != nil {
		return Validation{Status: StatusInvalid, Container: "macho", Reason: "invalid Mach-O container"}, true
	}
	defer f.Close()
	return validateMachOFile(f), true
}

func validateMachOFile(f *macho.File) Validation {
	if f == nil {
		return Validation{Status: StatusInvalid, Container: "macho", Reason: "missing Mach-O file"}
	}
	debugInfo := false
	for _, s := range f.Sections {
		if s == nil {
			continue
		}
		switch s.Name {
		case "__debug_info", "__debug_line", "__zdebug_info", "__zdebug_line":
			debugInfo = true
		}
	}
	arch := machoArch(f.Cpu)
	if !debugInfo {
		return Validation{
			Status: StatusRecognized, Container: "macho", Architecture: arch,
			Reason: "Mach-O container has no recognised DWARF section; frame rewriting remains unresolved",
		}
	}
	return Validation{
		Status: StatusRecognized, Container: "macho", Architecture: arch,
		HasDebugInfo: true,
		Reason:       "DWARF container recognised; frame rewriting remains unresolved",
	}
}

func elfArch(machine elf.Machine) string {
	switch machine {
	case elf.EM_X86_64:
		return "x86_64"
	case elf.EM_AARCH64:
		return "arm64"
	case elf.EM_ARM:
		return "arm"
	case elf.EM_386:
		return "x86"
	default:
		return fmt.Sprintf("machine-%d", machine)
	}
}

func machoArch(cpu macho.Cpu) string {
	switch cpu {
	case macho.CpuAmd64:
		return "x86_64"
	case macho.CpuArm64:
		return "arm64"
	case macho.CpuArm:
		return "arm"
	case macho.Cpu386:
		return "x86"
	default:
		return fmt.Sprintf("cpu-%d", cpu)
	}
}

// ValidateArtifact applies the currently supported format policy.  It keeps
// unsupported formats explicit instead of accidentally treating an Android or
// iOS artifact as a Dart symbols file.
func ValidateArtifact(platform, format string, content []byte) Validation {
	if platform != "flutter" || format != "dart_symbols" {
		return Validation{
			Status: StatusRetainedUnresolved,
			Reason: "format-specific resolver is not enabled for this artifact",
		}
	}
	return ValidateDartSymbols(content)
}

// IsRecognized reports whether the artifact has a known executable container.
// It is kept small for callers that only need a health badge.
func (v Validation) IsRecognized() bool {
	return v.Status == StatusRecognized
}

// ErrUnsupported is provided for future resolver callers without making the
// current read path depend on parser internals.
var ErrUnsupported = errors.New("native symbol format is unsupported")
