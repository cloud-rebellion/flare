package api

import (
	"os"
	"strings"
	"testing"
)

func TestNormalizeAPIKeyHygiene(t *testing.T) {
	mode, threshold, horizon, err := normalizeAPIKeyHygiene(" EXPIRING ", 2, 60)
	if err != nil || mode != "expiring" || threshold != 2 || horizon != 60 {
		t.Fatalf("normalized api-key hygiene = %q/%d/%d, err %v", mode, threshold, horizon, err)
	}
	tests := []struct {
		name      string
		mode      string
		threshold int32
		horizon   int32
	}{
		{"unknown mode", "revoked", 1, 60},
		{"missing mode", "", 1, 60},
		{"zero threshold", "unused", 0, 60},
		{"negative horizon", "unused", 1, 0},
		{"horizon too wide", "unused", 1, maxAPIKeyHygieneWindowMinutes + 1},
		{"threshold too wide", "unused", 1_000_001, 60},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := normalizeAPIKeyHygiene(tc.mode, tc.threshold, tc.horizon); err == nil {
				t.Fatal("invalid api-key hygiene rule was accepted")
			}
		})
	}
}

func TestAPIKeyHygieneReason(t *testing.T) {
	if got := apiKeyHygieneReason(0, "unused", 1, 60); got != "" {
		t.Fatalf("empty key series fired: %q", got)
	}
	if got := apiKeyHygieneReason(1, "unused", 2, 60); got != "" {
		t.Fatalf("below-threshold key series fired: %q", got)
	}
	if got := apiKeyHygieneReason(2, "unused", 2, 60); !strings.Contains(got, "unused key") || !strings.Contains(got, "older than 60m") {
		t.Fatalf("unused reason = %q", got)
	}
	if got := apiKeyHygieneReason(2, "expiring", 1, 120); !strings.Contains(got, "expiring within 120m") {
		t.Fatalf("expiring reason = %q", got)
	}
	for _, mode := range []string{"", "expired", "UNUSED"} {
		if got := apiKeyHygieneReason(2, mode, 1, 60); got != "" {
			t.Fatalf("invalid mode %q fired: %q", mode, got)
		}
	}
}

func TestAPIKeyHygieneQueryIsTenantScopedAndPayloadFree(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetAPIKeyHygieneStats")
	if start < 0 {
		t.Fatal("api-key hygiene query missing")
	}
	query := body[start:]
	if next := strings.Index(query[len("-- name: GetAPIKeyHygieneStats"):], "\n-- name:"); next >= 0 {
		query = query[:len("-- name: GetAPIKeyHygieneStats")+next]
	}
	if !strings.Contains(query, "org_id = sqlc.arg(org_id)::text") {
		t.Fatal("api-key hygiene query is missing the org tenant predicate")
	}
	if !strings.Contains(query, "last_used_at IS NULL OR last_used_at <= sqlc.arg(older_than)") ||
		!strings.Contains(query, "created_at <= sqlc.arg(older_than)") {
		t.Fatal("unused key mode must count never-used and idle keys only after an age guard")
	}
	for _, forbidden := range []string{"key_hash", "key_prefix", "created_by", "email"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("api-key hygiene query exposes credential field %q", forbidden)
		}
	}
}
