package sourcemaps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-sourcemap/sourcemap"
)

// ParseStatus is the bounded acceptance outcome persisted beside an uploaded
// source map. It deliberately has a small fixed vocabulary so an upstream
// parser message or map content never becomes tenant-visible metadata.
type ParseStatus string

const (
	ParseStatusUnvalidated     ParseStatus = "unvalidated"
	ParseStatusValid           ParseStatus = "valid"
	ParseStatusInvalidJSON     ParseStatus = "invalid_json"
	ParseStatusInvalidMappings ParseStatus = "invalid_mappings"
	ParseStatusUnsupported     ParseStatus = "unsupported"
)

// Validate checks the same source-map parser used by read-time symbolication.
// The upload path still stores every bounded document for compatibility; this
// result tells operators whether the retained bytes are usable by Flare's
// resolver. JSON is checked first so syntactically malformed documents do not
// get reported as a mappings failure. The parser is third-party code and can
// dereference an absent indexed-map section defensively, so a malformed map
// must never panic an ingest or dashboard process.
func Validate(content []byte) (status ParseStatus) {
	status = ParseStatusInvalidJSON
	if len(content) == 0 || !json.Valid(content) {
		return status
	}
	defer func() {
		if recover() != nil {
			status = ParseStatusInvalidMappings
		}
	}()
	if err := validateMapShape(content); err != nil {
		return ParseStatusInvalidMappings
	}
	if _, err := sourcemap.Parse("", content); err != nil {
		if strings.Contains(err.Error(), "only 3rd version is supported") {
			return ParseStatusUnsupported
		}
		return ParseStatusInvalidMappings
	}
	return ParseStatusValid
}

// validateMapShape closes two permissiveness gaps in the upstream parser: its
// VLQ decoder treats unknown bytes as zero, and an incomplete segment at EOF is
// silently retained. The source-map grammar allows only 1, 4, or 5 fields per
// segment, with base64-VLQ bytes in each field. Indexed maps recurse through
// each section's embedded map.
func validateMapShape(content []byte) error {
	var doc struct {
		Mappings *string `json:"mappings"`
		Sections []struct {
			Map json.RawMessage `json:"map"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return err
	}
	if len(doc.Sections) > 0 {
		for _, section := range doc.Sections {
			if len(section.Map) == 0 {
				return fmt.Errorf("source map section has no map")
			}
			if err := validateMapShape(section.Map); err != nil {
				return err
			}
		}
		return nil
	}
	if doc.Mappings == nil || !validMappingsSyntax(*doc.Mappings) {
		return fmt.Errorf("source map mappings are malformed")
	}
	return nil
}

func validMappingsSyntax(mappings string) bool {
	if mappings == "" {
		return false
	}
	for _, line := range strings.Split(mappings, ";") {
		if line == "" { // Empty generated lines are valid.
			continue
		}
		for _, segment := range strings.Split(line, ",") {
			if segment == "" {
				return false
			}
			fields := 0
			for i := 0; i < len(segment); {
				continuation := true
				for continuation {
					if i >= len(segment) {
						return false
					}
					value, ok := base64Value(segment[i])
					if !ok {
						return false
					}
					continuation = value&32 != 0
					i++
				}
				fields++
			}
			if fields != 1 && fields != 4 && fields != 5 {
				return false
			}
		}
	}
	return true
}

func base64Value(c byte) (byte, bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return c - 'A', true
	case c >= 'a' && c <= 'z':
		return c - 'a' + 26, true
	case c >= '0' && c <= '9':
		return c - '0' + 52, true
	case c == '+':
		return 62, true
	case c == '/':
		return 63, true
	default:
		return 0, false
	}
}
