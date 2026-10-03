package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestConnectorSyncHealthResponseOmitsCursorAndTenantIdentifiers(t *testing.T) {
	now := time.Now().UTC()
	row := &generated.ListConnectorSyncHealthByOrgRow{
		ID: "sync-1", ConnectionID: "conn-1", Provider: "hubspot", InstallationName: "Production",
		ConnectionStatus: "active", Operation: "account_details", State: "succeeded",
		CursorConfigured: true, CursorVersion: 4, AttemptCount: 5,
		LastStartedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		LastCompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
		LastSuccessAt:   pgtype.Timestamptz{Time: now, Valid: true},
		LastErrorCode:   "", LastSyncLagMs: pgtype.Int8{Int64: 123, Valid: true},
		LeaseActive: false, UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
	body, err := json.Marshal(toConnectorSyncHealthResponse(row))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	encoded := string(body)
	for _, forbidden := range []string{"cursor_ciphertext", "secret", "org_id", "tenant"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("response contains %q: %s", forbidden, encoded)
		}
	}
	for _, want := range []string{`"cursor_configured":true`, `"cursor_version":4`, `"attempt_count":5`, `"last_sync_lag_ms":123`} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("response missing %q: %s", want, encoded)
		}
	}
}

func TestConnectorSyncHealthLimitIsBounded(t *testing.T) {
	for raw, want := range map[string]int{"": 100, "0": 100, "-4": 100, "bad": 100, "7": 7, "999": 100} {
		r := httptest.NewRequest("GET", "/api/connector-sync-health?limit="+raw, nil)
		if got := connectorSyncHealthLimit(r); got != want {
			t.Errorf("limit %q = %d, want %d", raw, got, want)
		}
	}
}

func TestConnectorSyncMigrationAndRoute(t *testing.T) {
	migration, err := os.ReadFile("../db/migrations/059_connector_sync_states.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	ddl := string(migration)
	for _, want := range []string{
		"org_id              TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE",
		"connection_id       TEXT NOT NULL",
		"cursor_ciphertext   TEXT NOT NULL DEFAULT ''",
		"lease_owner         TEXT NOT NULL DEFAULT ''",
		"FOREIGN KEY (connection_id, org_id)",
		"UNIQUE (org_id, connection_id, operation)",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("migration missing %q", want)
		}
	}
	router, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router: %v", err)
	}
	if !strings.Contains(string(router), `r.Get("/connector-sync-health", s.handleListConnectorSyncHealth)`) {
		t.Fatal("router missing connector sync health route")
	}
}
