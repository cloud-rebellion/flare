package api

import (
	"os"
	"strings"
	"testing"
)

func TestDeployFailureReason(t *testing.T) {
	if got := deployFailureReason(0, 1, 30); got != "" {
		t.Fatalf("empty deploy failure series fired: %q", got)
	}
	if got := deployFailureReason(2, 3, 30); got != "" {
		t.Fatalf("below-threshold deploy failures fired: %q", got)
	}
	if got := deployFailureReason(3, 3, 30); !strings.Contains(got, "failed or was blocked") {
		t.Fatalf("expected deploy failure alert, got %q", got)
	}
	for _, tc := range []struct {
		failures          int64
		threshold, window int32
	}{
		{failures: 1, threshold: 0, window: 30},
		{failures: 1, threshold: 1, window: 0},
		{failures: 1, threshold: 1, window: 721},
		{failures: 1, threshold: 1_000_001, window: 30},
	} {
		if got := deployFailureReason(tc.failures, tc.threshold, tc.window); got != "" {
			t.Fatalf("invalid deploy failure bounds fired: %q for %#v", got, tc)
		}
	}
}

func TestDeployFailureQueryIsTenantAndProjectScoped(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetDeployFailureStats")
	if start < 0 {
		t.Fatal("deploy failure query missing")
	}
	section := body[start:]
	if end := strings.Index(section[len("-- name: GetDeployFailureStats"):], "-- name:"); end >= 0 {
		section = section[:len("-- name: GetDeployFailureStats")+end]
	}
	for _, want := range []string{
		"project_id = sqlc.arg(project_id)::text",
		"org_id = sqlc.arg(org_id)::text",
		"stage = 'deploy'",
		"status IN ('failed', 'blocked')",
		"created_at >= sqlc.arg(since)",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("deploy failure query missing %q", want)
		}
	}
	for _, forbidden := range []string{"payload", "stacktrace", "source_revision", "error_code"} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("deploy failure query selected raw field %q", forbidden)
		}
	}
	watchStart := strings.Index(body, "-- name: ListWatchdogRules")
	if watchStart < 0 || !strings.Contains(body[watchStart:], "'deploy_failure'") {
		t.Fatal("watchdog rule list does not include deploy_failure")
	}
}
