package api

import (
	"os"
	"strings"
	"testing"
)

func TestNormalizeIngestAcceptanceBounds(t *testing.T) {
	if threshold, window, err := normalizeIngestAcceptance(3, 15); err != nil || threshold != 3 || window != 15 {
		t.Fatalf("normalizeIngestAcceptance = %d/%d/%v", threshold, window, err)
	}
	for _, tc := range []struct {
		threshold, window int32
	}{
		{0, 15},
		{1_000_001, 15},
		{1, 0},
		{1, maxIngestAcceptanceWindowMinutes + 1},
	} {
		if _, _, err := normalizeIngestAcceptance(tc.threshold, tc.window); err == nil {
			t.Fatalf("normalizeIngestAcceptance(%d,%d) accepted invalid input", tc.threshold, tc.window)
		}
	}
}

func TestIngestAcceptanceReasonUsesAggregateAndBounds(t *testing.T) {
	stats := int64(3)
	if got := ingestAcceptanceReason(stats, 2, 15); !strings.Contains(got, "3 incomplete") || !strings.Contains(got, "older than 15m") {
		t.Fatalf("ingest acceptance reason = %q", got)
	}
	for _, tc := range []struct {
		threshold, window int32
	}{
		{4, 15},
		{2, 0},
		{2, maxIngestAcceptanceWindowMinutes + 1},
		{1_000_001, 15},
	} {
		if got := ingestAcceptanceReason(stats, tc.threshold, tc.window); got != "" {
			t.Fatalf("invalid ingest acceptance bounds fired: %q for %#v", got, tc)
		}
	}
	if got := ingestAcceptanceReason(0, 1, 15); got != "" {
		t.Fatalf("nil ingest acceptance stats fired: %q", got)
	}
}

func TestIngestAcceptanceWatchdogQueryIsTenantScopedAndPayloadFree(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetIngestAcceptanceStats")
	if start < 0 {
		t.Fatal("ingest acceptance query missing")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: GetIngestAcceptanceStats"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: GetIngestAcceptanceStats")+next]
	}
	for _, want := range []string{
		"project_id = sqlc.arg(project_id)::text",
		"org_id = sqlc.arg(org_id)::text",
		"outcome = 'in_progress'",
		"completed_at IS NULL",
		"received_at < sqlc.arg(cutoff)",
		"count(*)::bigint",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("ingest acceptance query missing %q", want)
		}
	}
	for _, forbidden := range []string{"payload", "body", "credential", "request_id"} {
		if strings.Contains(strings.ToLower(section), forbidden) {
			t.Fatalf("ingest acceptance query exposed raw field %q", forbidden)
		}
	}
	watchStart := strings.Index(body, "-- name: ListWatchdogRules")
	if watchStart < 0 || !strings.Contains(body[watchStart:], "'ingest_acceptance'") {
		t.Fatal("watchdog rule list does not include ingest_acceptance")
	}
}

func TestIngestAcceptanceMigrationAddsPartialHealthIndex(t *testing.T) {
	raw, err := os.ReadFile("../db/migrations/061_ingest_receipt_health.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"idx_ingest_receipts_in_progress_health",
		"outcome = 'in_progress'",
		"completed_at IS NULL",
		"received_at DESC",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("migration missing %q", want)
		}
	}
}
