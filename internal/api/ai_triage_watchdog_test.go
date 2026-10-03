package api

import (
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeAITriage(t *testing.T) {
	if mode, threshold, window, err := normalizeAITriage(" STUCK ", 2, 60); err != nil || mode != "stuck" || threshold != 2 || window != 60 {
		t.Fatalf("normalizeAITriage = %q/%d/%d/%v", mode, threshold, window, err)
	}
	for _, tc := range []struct {
		mode              string
		threshold, window int32
	}{
		{"", 1, 5},
		{"pending", 1, 5},
		{"failed", 0, 5},
		{"failed", 1_000_001, 5},
		{"failed", 1, 0},
		{"failed", 1, maxAITriageWindowMinutes + 1},
	} {
		if _, _, _, err := normalizeAITriage(tc.mode, tc.threshold, tc.window); err == nil {
			t.Fatalf("normalizeAITriage(%q,%d,%d) accepted invalid input", tc.mode, tc.threshold, tc.window)
		}
	}
}

func TestAITriageReasonUsesExplicitModeAndBounds(t *testing.T) {
	stats := &generated.GetAITriageStatsRow{FailedCount: 2, FailedErrorCount: 2, StuckCount: 3}
	if got := aiTriageReason(stats, "failed", 2, 15); !strings.Contains(got, "failed 2") || !strings.Contains(got, "2 with safe error codes") {
		t.Fatalf("failed AI triage reason = %q", got)
	}
	if got := aiTriageReason(stats, "stuck", 3, 30); !strings.Contains(got, "3 stuck") || !strings.Contains(got, "older than 30m") {
		t.Fatalf("stuck AI triage reason = %q", got)
	}
	for _, tc := range []struct {
		mode              string
		threshold, window int32
	}{
		{"failed", 3, 15},
		{"stuck", 4, 15},
		{"unknown", 1, 15},
		{"failed", 1, 0},
		{"failed", 1, 721},
		{"failed", 1_000_001, 15},
	} {
		if got := aiTriageReason(stats, tc.mode, tc.threshold, tc.window); got != "" {
			t.Fatalf("invalid AI triage bounds fired: %q for %#v", got, tc)
		}
	}
}

func TestAITriageWatchdogQueryIsTenantScopedAndPayloadFree(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetAITriageStats")
	if start < 0 {
		t.Fatal("AI triage query missing")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: GetAITriageStats"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: GetAITriageStats")+next]
	}
	queryStart := strings.Index(section, "SELECT count(*) FILTER")
	if queryStart < 0 {
		t.Fatal("AI triage query body missing")
	}
	query := section[queryStart:]
	if end := strings.Index(query[1:], "\n-- "); end >= 0 {
		query = query[:1+end]
	}
	for _, want := range []string{
		"project_id = sqlc.arg(project_id)::text",
		"org_id = sqlc.arg(org_id)::text",
		"operation = 'triage'",
		"status = 'failed'",
		"status = 'started'",
		"error_code IS NOT NULL",
		"completed_at IS NULL",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("AI triage query missing %q", want)
		}
	}
	for _, forbidden := range []string{"prompt", "completion", "payload", "stacktrace", "provider_error"} {
		if strings.Contains(strings.ToLower(query), forbidden) {
			t.Fatalf("AI triage query exposed raw field %q", forbidden)
		}
	}
	watchStart := strings.Index(body, "-- name: ListWatchdogRules")
	if watchStart < 0 || !strings.Contains(body[watchStart:], "'ai_triage'") {
		t.Fatal("watchdog rule list does not include ai_triage")
	}
}

func TestAITriageMigrationAddsBoundedModeAndIndex(t *testing.T) {
	raw, err := os.ReadFile("../db/migrations/053_ai_triage_alert_rules.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"ADD COLUMN ai_triage_mode TEXT NOT NULL DEFAULT ''",
		"CHECK (ai_triage_mode IN ('', 'failed', 'stuck'))",
		"idx_ai_action_receipts_watchdog",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("migration missing %q", want)
		}
	}
}
