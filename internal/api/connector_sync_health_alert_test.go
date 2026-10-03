package api

import (
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeConnectorSyncHealthBoundsAndDefaults(t *testing.T) {
	provider, operation, mode, threshold, window, err := normalizeConnectorSyncHealth("HubSpot", "account_details", "", 2, 15)
	if err != nil || provider != "hubspot" || operation != "account_details" || mode != "overdue" || threshold != 2 || window != 15 {
		t.Fatalf("normalizeConnectorSyncHealth = %q/%q/%q/%d/%d/%v", provider, operation, mode, threshold, window, err)
	}
	for _, tc := range []struct {
		provider, operation, mode string
		threshold, window         int32
	}{
		{"", "account_details", "failed", 1, 5},
		{"hubspot", "", "failed", 1, 5},
		{"hubspot", "account_details", "unknown", 1, 5},
		{"hubspot", "account_details", "failed", 0, 5},
		{"hubspot", "account_details", "failed", 1_000_001, 5},
		{"hubspot", "account_details", "failed", 1, 0},
		{"hubspot", "account_details", "failed", 1, 721},
	} {
		if _, _, _, _, _, err := normalizeConnectorSyncHealth(tc.provider, tc.operation, tc.mode, tc.threshold, tc.window); err == nil {
			t.Fatalf("normalizeConnectorSyncHealth accepted invalid input %#v", tc)
		}
	}
}

func TestConnectorSyncHealthReasonUsesOnlySelectedMode(t *testing.T) {
	stats := &generated.GetConnectorSyncHealthStatsRow{FailedCount: 2, OverdueCount: 3}
	if got := connectorSyncHealthReason(stats, "hubspot", "account_details", "conn-1", "failed", 2, 15); !strings.Contains(got, "2 failed sync state") || !strings.Contains(got, "connection:conn-1") {
		t.Fatalf("failed reason = %q", got)
	}
	if got := connectorSyncHealthReason(stats, "hubspot", "account_details", "conn-1", "overdue", 3, 15); !strings.Contains(got, "3 overdue sync state") {
		t.Fatalf("overdue reason = %q", got)
	}
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"failed", ""},
		{"overdue", ""},
		{"unknown", ""},
	} {
		if got := connectorSyncHealthReason(stats, "hubspot", "account_details", "conn-1", tc.mode, 4, 15); got != tc.want {
			t.Fatalf("reason for %#v = %q", tc, got)
		}
	}
	if got := connectorSyncHealthReason(nil, "hubspot", "account_details", "conn-1", "failed", 1, 15); got != "" {
		t.Fatalf("nil stats fired: %q", got)
	}
}

func TestConnectorSyncHealthQueryIsTenantAndInstallationScoped(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetConnectorSyncHealthStats")
	if start < 0 {
		t.Fatal("sync health query missing")
	}
	section := body[start:]
	if end := strings.Index(section, ";"); end >= 0 {
		section = section[:end+1]
	}
	for _, want := range []string{
		"s.org_id = sqlc.arg(org_id)::text",
		"s.connection_id = sqlc.arg(connection_id)::text",
		"c.provider = sqlc.arg(provider)::text",
		"s.operation = sqlc.arg(operation)::text",
		"s.state = 'failed'",
		"s.next_run_at < sqlc.arg(cutoff)",
		"s.lease_expires_at < sqlc.arg(cutoff)",
		"c.status IN ('active', 'pending', 'error')",
		"count(*) FILTER",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("sync health query missing %q", want)
		}
	}
	for _, forbidden := range []string{"payload", "secret_ciphertext", "cursor_ciphertext"} {
		if strings.Contains(strings.ToLower(section), forbidden) {
			t.Fatalf("sync health query exposes raw field %q", forbidden)
		}
	}
}

func TestConnectorSyncHealthMigrationBoundsMode(t *testing.T) {
	raw, err := os.ReadFile("../db/migrations/063_connector_sync_health_alerts.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"ADD COLUMN connector_sync_mode TEXT NOT NULL DEFAULT ''",
		"connector_sync_mode IN ('', 'failed', 'overdue')",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("migration missing %q", want)
		}
	}
}
