package analytics

import "testing"

func TestBoundedExportErrorCodeVocabulary(t *testing.T) {
	for _, code := range []string{"partition_probe", "source_read", "cold_write", "cold_stat", "verify"} {
		if got := boundedExportErrorCode(code); got != code {
			t.Fatalf("boundedExportErrorCode(%q) = %q", code, got)
		}
	}
	for _, code := range []string{"", "provider secret leaked", "very-long-untrusted-error"} {
		if got := boundedExportErrorCode(code); got != "unknown" {
			t.Fatalf("boundedExportErrorCode(%q) = %q, want unknown", code, got)
		}
	}
}

func TestExportErrorCodesHaveNoPayloadVocabulary(t *testing.T) {
	for code := range exportErrorCodes {
		for _, forbidden := range []string{"payload", "secret", "token", "url", "password", "body"} {
			if code == forbidden {
				t.Fatalf("durable exporter error code %q would expose provider detail", code)
			}
		}
	}
}
