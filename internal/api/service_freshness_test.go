package api

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestNormalizeServiceFreshnessRequiresBoundedExactDimension(t *testing.T) {
	service, release, window, err := normalizeServiceFreshness(" api-web ", " 2026.10.03 ", 30)
	if err != nil || service != "api-web" || release != "2026.10.03" || window != 30 {
		t.Fatalf("normalized service freshness = %q/%q/%d, err %v", service, release, window, err)
	}
	for _, tc := range []struct {
		name    string
		service string
		release string
		window  int32
	}{
		{"missing service", "", "v1", 30},
		{"service too long", strings.Repeat("x", 65), "v1", 30},
		{"service control", "api\nweb", "v1", 30},
		{"missing release", "api-web", "", 30},
		{"release too long", "api-web", strings.Repeat("x", 129), 30},
		{"zero window", "api-web", "v1", 0},
		{"window too wide", "api-web", "v1", 721},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := normalizeServiceFreshness(tc.service, tc.release, tc.window); err == nil {
				t.Fatal("invalid service freshness rule was accepted")
			}
		})
	}
}

func TestServiceFreshnessReasonUsesExactBaselineAndHeartbeat(t *testing.T) {
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	baseline := pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	created := pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	if got := serviceFreshnessReason(pgtype.Timestamptz{}, "api-web", "v1", baseline, created, now, 30); !strings.Contains(got, "no heartbeat") {
		t.Fatalf("empty series reason = %q", got)
	}
	recentBaseline := pgtype.Timestamptz{Time: now.Add(-5 * time.Minute), Valid: true}
	if got := serviceFreshnessReason(pgtype.Timestamptz{}, "api-web", "v1", recentBaseline, created, now, 30); got != "" {
		t.Fatalf("empty series fired before baseline grace: %q", got)
	}
	last := pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	if got := serviceFreshnessReason(last, "api-web", "v1", baseline, created, now, 30); !strings.Contains(got, "heartbeat is stale") {
		t.Fatalf("stale series reason = %q", got)
	}
	last = pgtype.Timestamptz{Time: now.Add(-5 * time.Minute), Valid: true}
	if got := serviceFreshnessReason(last, "api-web", "v1", baseline, created, now, 30); got != "" {
		t.Fatalf("fresh series fired: %q", got)
	}
	// A heartbeat exactly at the baseline is a real heartbeat, but is stale
	// after the grace period rather than being mislabeled as an empty series.
	last = pgtype.Timestamptz{Time: baseline.Time, Valid: true}
	if got := serviceFreshnessReason(last, "api-web", "v1", baseline, created, now, 30); !strings.Contains(got, "heartbeat is stale") {
		t.Fatalf("baseline heartbeat reason = %q", got)
	}
	if got := serviceFreshnessReason(last, "api-web", "other", baseline, created, now, 30); !strings.Contains(got, "release other") {
		t.Fatalf("reason missing exact release dimension: %q", got)
	}
	if got := serviceFreshnessReason(last, "api-web", "v1", baseline, created, now, 180); got != "" {
		t.Fatalf("series fired before grace: %q", got)
	}
}

func TestServiceFreshnessQueryIsTenantScopedAndPayloadFree(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog queries: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetServiceFreshnessStats")
	if start < 0 {
		t.Fatal("service freshness watchdog query is missing")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: GetServiceFreshnessStats"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: GetServiceFreshnessStats")+next]
	}
	for _, want := range []string{
		"e.project_id = sqlc.arg(project_id)::text",
		"e.org_id = sqlc.arg(org_id)::text",
		"e.release = sqlc.arg(release_version)::text",
		"e.message = ('service-up:' || sqlc.arg(service_name)::text)",
		"e.received_at >= sqlc.arg(baseline_at)",
		"max(e.received_at)",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("service freshness query missing %q", want)
		}
	}
	for _, forbidden := range []string{"payload", "stacktrace", "credentials"} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("service freshness query exposes event field %q", forbidden)
		}
	}
}

func TestServiceFreshnessRuleMigrationBoundsSelector(t *testing.T) {
	raw, err := os.ReadFile("../db/migrations/054_service_freshness_alert_rules.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"ADD COLUMN service_name TEXT NOT NULL DEFAULT ''",
		"ADD COLUMN service_freshness_baseline_at TIMESTAMPTZ NOT NULL DEFAULT now()",
		"octet_length(service_name) <= 64",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("service freshness migration missing %q", want)
		}
	}
}
