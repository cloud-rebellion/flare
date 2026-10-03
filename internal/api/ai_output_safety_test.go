package api

import (
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/ai"
)

func TestSanitizeTriageOutputRedactsProviderOutputAndCapsIt(t *testing.T) {
	const secret = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9.sIgNaTuRe1234"
	got := sanitizeTriageOutput("The provider echoed " + secret + " and then replied.")
	if strings.Contains(got, secret) {
		t.Fatalf("provider output retained a credential: %q", got)
	}
	if !strings.Contains(got, "[jwt]") {
		t.Fatalf("provider output was not scrubbed: %q", got)
	}

	got = sanitizeTriageOutput(strings.Repeat("x", ai.MaxUntrustedBytes*3))
	if len(got) > ai.MaxUntrustedBytes+len("\n[truncated]") {
		t.Fatalf("provider output was not capped: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Fatal("provider output truncation is silent")
	}
}
