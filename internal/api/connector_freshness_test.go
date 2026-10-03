package api

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestNormalizeConnectorFreshness(t *testing.T) {
	provider, operation, channel, window, err := normalizeConnectorFreshness(" HubSpot ", " contacts.list ", " HTTP ", 15)
	if err != nil || provider != "hubspot" || operation != "contacts.list" || channel != "http" || window != 15 {
		t.Fatalf("normalized connector freshness = %q/%q/%q/%d, err %v", provider, operation, channel, window, err)
	}
	if provider, operation, channel, window, err = normalizeConnectorFreshness("hubspot", "", "", 1); err != nil || provider != "hubspot" || operation != "" || channel != "" || window != 1 {
		t.Fatalf("normalized empty selectors = %q/%q/%q/%d, err %v", provider, operation, channel, window, err)
	}
	for _, tc := range []struct {
		name      string
		provider  string
		operation string
		channel   string
		window    int32
	}{
		{"missing provider", "", "sync", "http", 15},
		{"provider too long", strings.Repeat("x", 33), "sync", "http", 15},
		{"operation too long", "hubspot", strings.Repeat("x", 65), "http", 15},
		{"channel too long", "hubspot", "sync", strings.Repeat("x", 33), 15},
		{"zero window", "hubspot", "sync", "http", 0},
		{"window too wide", "hubspot", "sync", "http", 721},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := normalizeConnectorFreshness(tc.provider, tc.operation, tc.channel, tc.window); err == nil {
				t.Fatal("invalid freshness rule was accepted")
			}
		})
	}
}

func TestConnectorFreshnessReasonUsesBaselineAndLatestAttempt(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	baseline := pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	if got := connectorFreshnessReason(pgtype.Timestamptz{}, "hubspot", "sync", "http", baseline, pgtype.Timestamptz{}, now, 30); !strings.Contains(got, "no completed attempt") {
		t.Fatalf("empty series reason = %q", got)
	}
	if got := connectorFreshnessReason(pgtype.Timestamptz{}, "hubspot", "sync", "http", baseline, pgtype.Timestamptz{}, now, 180); got != "" {
		t.Fatalf("empty series fired before baseline cadence: %q", got)
	}
	last := pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	if got := connectorFreshnessReason(last, "hubspot", "sync", "http", baseline, pgtype.Timestamptz{}, now, 30); !strings.Contains(got, "last completed") {
		t.Fatalf("stale series reason = %q", got)
	}
	last = pgtype.Timestamptz{Time: now.Add(-5 * time.Minute), Valid: true}
	if got := connectorFreshnessReason(last, "hubspot", "sync", "http", baseline, pgtype.Timestamptz{}, now, 30); got != "" {
		t.Fatalf("fresh series fired: %q", got)
	}
	if got := connectorFreshnessReason(last, "", "sync", "http", baseline, pgtype.Timestamptz{}, now, 30); got != "" {
		t.Fatalf("missing provider fired: %q", got)
	}
}

func TestConnectorFreshnessQueryIsTenantScopedAndBaselineGated(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog queries: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetConnectorFreshnessStats")
	if start < 0 {
		t.Fatal("freshness watchdog query is missing")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: GetConnectorFreshnessStats"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: GetConnectorFreshnessStats")+next]
	}
	for _, want := range []string{
		"COALESCE(project_id, '') = sqlc.arg(project_id)::text",
		"org_id = sqlc.arg(org_id)::text",
		"provider = sqlc.arg(provider)::text",
		"sqlc.arg(connection_id)::text = '' OR connection_id = sqlc.arg(connection_id)::text",
		"completed_at >= sqlc.arg(baseline_at)",
		"max(completed_at)",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("freshness query missing %q", want)
		}
	}
	for _, forbidden := range []string{"payload", "response", "credential"} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("freshness query exposes connector field %q", forbidden)
		}
	}
}
