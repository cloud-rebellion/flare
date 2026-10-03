package connectorsync

import (
	"strings"
	"testing"
)

func TestValidateOperationAndWorkerIDBounds(t *testing.T) {
	for _, value := range []string{"", "sync worker", "sync/worker", strings.Repeat("x", MaxOperationBytes+1)} {
		if err := ValidateOperation(value); err == nil {
			t.Errorf("ValidateOperation(%q) accepted invalid value", value)
		}
	}
	if err := ValidateOperation("crm.contacts"); err != nil {
		t.Fatalf("valid operation rejected: %v", err)
	}
	for _, value := range []string{"", "worker\n1", strings.Repeat("w", MaxWorkerIDBytes+1)} {
		if err := ValidateWorkerID(value); err == nil {
			t.Errorf("ValidateWorkerID(%q) accepted invalid value", value)
		}
	}
}

func TestValidateCursorBoundsAndControls(t *testing.T) {
	for _, value := range []string{"", "cursor\nleak", strings.Repeat("c", MaxCursorBytes+1)} {
		if err := ValidateCursor(value); err == nil {
			t.Errorf("ValidateCursor accepted invalid value %q", value)
		}
	}
	if err := ValidateCursor("opaque-page-token.v2"); err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
}
