package api

import (
	"os"
	"strings"
	"testing"
)

func TestRollbackReason(t *testing.T) {
	if got := rollbackReason(0, 1, 30); got != "" {
		t.Fatalf("empty rollback series fired: %q", got)
	}
	if got := rollbackReason(1, 2, 30); got != "" {
		t.Fatalf("below-threshold rollback series fired: %q", got)
	}
	if got := rollbackReason(2, 2, 30); !strings.Contains(got, "Explicit rollback recorded") {
		t.Fatalf("expected explicit rollback alert, got %q", got)
	}
	for _, tc := range []struct {
		rollbacks         int64
		threshold, window int32
	}{
		{rollbacks: 1, threshold: 0, window: 30},
		{rollbacks: 1, threshold: 1, window: 0},
		{rollbacks: 1, threshold: 1, window: 721},
		{rollbacks: 1, threshold: 1_000_001, window: 30},
	} {
		if got := rollbackReason(tc.rollbacks, tc.threshold, tc.window); got != "" {
			t.Fatalf("invalid rollback bounds fired: %q for %#v", got, tc)
		}
	}
}

func TestRollbackQueryIsExplicitAndTenantScoped(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetRollbackStats")
	if start < 0 {
		t.Fatal("rollback query missing")
	}
	section := body[start:]
	if end := strings.Index(section[len("-- name: GetRollbackStats"):], "-- name:"); end >= 0 {
		section = section[:len("-- name: GetRollbackStats")+end]
	}
	for _, want := range []string{
		"project_id = sqlc.arg(project_id)::text",
		"org_id = sqlc.arg(org_id)::text",
		"stage = 'rollback'",
		"status IN ('passed', 'failed', 'blocked')",
		"created_at >= sqlc.arg(since)",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("rollback query missing %q", want)
		}
	}
	for _, forbidden := range []string{"payload", "stacktrace", "source_revision", "error_code"} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("rollback query selected raw field %q", forbidden)
		}
	}
	watchStart := strings.Index(body, "-- name: ListWatchdogRules")
	if watchStart < 0 || !strings.Contains(body[watchStart:], "'rollback'") {
		t.Fatal("watchdog rule list does not include rollback")
	}
}
