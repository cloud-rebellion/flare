package api

import (
	"os"
	"strings"
	"testing"
)

func TestConnectorConnectionAlertSelectorIsTenantChecked(t *testing.T) {
	src, err := os.ReadFile("alerts_handlers.go")
	if err != nil {
		t.Fatalf("read alerts handler: %v", err)
	}
	body := string(src)
	for _, want := range []string{
		"connector_connection_id is only valid for connector rules",
		"GetConnectorConnectionByID",
		"OrgID: proj.OrgID",
		"connection.Provider, connectorProvider",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("alert handler missing connection selector guard %q", want)
		}
	}
	if !strings.Contains(body, "ConnectorConnectionID: connectorConnectionID") {
		t.Fatal("alert rule create does not persist connector connection selector")
	}
}

func TestConnectorConnectionAlertMigrationBoundsSelector(t *testing.T) {
	raw, err := os.ReadFile("../db/migrations/062_connector_alert_connection.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"ADD COLUMN connector_connection_id TEXT NOT NULL DEFAULT ''",
		"length(connector_connection_id) <= 128",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("migration missing %q", want)
		}
	}
}
