package native_symbols

import (
	"bytes"
	"debug/elf"
	"os"
	"runtime"
	"testing"
)

func TestValidateDartSymbolsRejectsEmptyAndUnknownBytes(t *testing.T) {
	if got := ValidateDartSymbols(nil); got.Status != StatusInvalid {
		t.Fatalf("empty status = %q, want %q", got.Status, StatusInvalid)
	}
	if got := ValidateDartSymbols([]byte("not-a-debug-container")); got.Status != StatusUnsupported {
		t.Fatalf("unknown status = %q, want %q", got.Status, StatusUnsupported)
	}
}

func TestValidateArtifactDoesNotClaimUnsupportedFormats(t *testing.T) {
	got := ValidateArtifact("android", "proguard", []byte("class a -> b:"))
	if got.Status != StatusRetainedUnresolved {
		t.Fatalf("status = %q, want %q", got.Status, StatusRetainedUnresolved)
	}
	if got.IsRecognized() {
		t.Fatal("unsupported format was reported as recognized")
	}
}

func TestValidateDartSymbolsRecognizesELFDebugContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test executable is not ELF on Windows")
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read test executable: %v", err)
	}
	// Keep this test honest if the host test runner is not ELF (for example,
	// macOS); the deterministic Mach-O magic-path test below covers that branch.
	if len(content) < 4 || !bytes.Equal(content[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		t.Skip("test executable is not ELF on this host")
	}
	if _, err := elf.NewFile(bytes.NewReader(content)); err != nil {
		t.Fatalf("test executable is not parseable ELF: %v", err)
	}
	got := ValidateDartSymbols(content)
	if !got.IsRecognized() {
		t.Fatalf("ELF status = %q (%s), want recognized", got.Status, got.Reason)
	}
	if got.Container != "elf" || got.Architecture == "" {
		t.Fatalf("ELF metadata = %#v", got)
	}
}

func TestValidateDartSymbolsRecognizesMalformedMachOContainer(t *testing.T) {
	// A Mach-O magic value is enough to enter the format-specific parser, while
	// this deliberately truncated fixture must remain invalid. This proves that
	// arbitrary four-byte input cannot receive a recognized/unresolved badge.
	got := ValidateDartSymbols([]byte{0xfe, 0xed, 0xfa, 0xcf})
	if got.Container != "macho" || got.Status != StatusInvalid {
		t.Fatalf("Mach-O validation = %#v, want invalid macho", got)
	}
}

func TestValidationNeverRetainsArtifactBytes(t *testing.T) {
	content := []byte{0x7f, 'E', 'L', 'F', 1, 2, 3, 4}
	_ = ValidateDartSymbols(content)
	content[0] = 0
	got := ValidateDartSymbols(content)
	if got.Status == StatusRecognized {
		t.Fatal("validation result unexpectedly retained or reused mutable artifact bytes")
	}
}
