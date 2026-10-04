package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/secretbox"
)

func TestNormalizeConnectorConnectionValuesBoundsAndCanonicalScopes(t *testing.T) {
	got, err := normalizeConnectorConnectionValues("hubspot", "install-1", "Production", "", []string{"crm.read", "crm.write", "crm.read"})
	if err != nil {
		t.Fatalf("normalize valid connection: %v", err)
	}
	if got.status != "pending" || string(got.scopes) != `["crm.read","crm.write"]` {
		t.Fatalf("normalized connection = %#v, scopes %s", got, got.scopes)
	}
	for name, tc := range map[string]struct {
		provider string
		install  string
		name     string
		status   string
		scopes   []string
	}{
		"provider token": {provider: "hubspot?secret", install: "x"},
		"empty install":  {provider: "hubspot"},
		"bad status":     {provider: "hubspot", install: "x", status: "connected"},
		"bad scope":      {provider: "hubspot", install: "x", scopes: []string{"crm read"}},
		"too many scopes": {provider: "hubspot", install: "x", scopes: func() []string {
			return make([]string, maxConnectorScopes+1)
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeConnectorConnectionValues(tc.provider, tc.install, tc.name, tc.status, tc.scopes); err == nil {
				t.Fatal("invalid connection was accepted")
			}
		})
	}
}

func TestNormalizeConnectorSecretDistinguishesKeepAndReplace(t *testing.T) {
	if value, supplied, err := normalizeConnectorSecret(nil); err != nil || supplied || value != "" {
		t.Fatalf("omitted secret = %q/%v/%v, want empty/not supplied", value, supplied, err)
	}
	if _, supplied, err := normalizeConnectorSecret(stringPtr("")); err == nil || !supplied {
		t.Fatalf("empty supplied secret = supplied %v, err %v; want rejection", supplied, err)
	}
	if value, supplied, err := normalizeConnectorSecret(stringPtr("tok_live_123")); err != nil || !supplied || value != "tok_live_123" {
		t.Fatalf("replacement secret = %q/%v/%v", value, supplied, err)
	}
	if _, _, err := normalizeConnectorSecret(stringPtr("line\nfeed")); err == nil {
		t.Fatal("control-character secret was accepted")
	}
}

func TestConnectorConnectionResponseIsMetadataOnly(t *testing.T) {
	now := time.Now().UTC()
	row := &generated.CreateConnectorConnectionRow{
		ID: "conn-1", OrgID: "org-secret-must-not-escape", Provider: "hubspot",
		InstallationID: "install-1", InstallationName: "Production", Status: "active",
		Scopes: json.RawMessage(`["crm.read"]`), CredentialConfigured: true,
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
	encoded, err := json.Marshal(connectorConnectionResponseFromCreate(row))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	body := string(encoded)
	for _, forbidden := range []string{"secret_ciphertext", "org_id", "org-secret-must-not-escape"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metadata response contains %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"credential_configured":true`) || !strings.Contains(body, `"scopes":["crm.read"]`) {
		t.Fatalf("metadata response lost safe fields: %s", body)
	}
}

func TestConnectorConnectionSecretUsesSecretbox(t *testing.T) {
	cipher := secretbox.New("connector-test-key")
	stored := cipher.Encrypt("provider-token")
	if stored == "provider-token" || !strings.HasPrefix(stored, "enc:v1:") {
		t.Fatalf("secret was not encrypted at rest: %q", stored)
	}
	if got, err := cipher.Decrypt(stored); err != nil || got != "provider-token" {
		t.Fatalf("secret round trip = %q/%v", got, err)
	}
}

func TestConnectorCredentialWriteFailsClosedWithoutEncryption(t *testing.T) {
	for _, s := range []*Server{{}, {secrets: secretbox.New("")}} {
		if stored, err := s.encryptConnectorSecret("provider-token"); err == nil || stored != "" {
			t.Fatalf("unencrypted connector write accepted: stored %q, error %v", stored, err)
		}
		// A pending connection without a credential and a metadata-only PATCH
		// still have no new secret to protect.
		if stored, err := s.encryptConnectorSecret(""); err != nil || stored != "" {
			t.Fatalf("empty connector secret = %q, %v", stored, err)
		}
	}
	s := &Server{secrets: secretbox.New("connector-test-key")}
	stored, err := s.encryptConnectorSecret("provider-token")
	if err != nil || !strings.HasPrefix(stored, "enc:v1:") || stored == "provider-token" {
		t.Fatalf("encrypted connector write = %q, %v", stored, err)
	}
	if got, err := s.secrets.Decrypt(stored); err != nil || got != "provider-token" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}

func TestConnectorConnectionMigrationAndRoutes(t *testing.T) {
	migration, err := os.ReadFile("../db/migrations/057_connector_connections.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	ddl := string(migration)
	for _, want := range []string{
		"org_id              TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE",
		"provider            TEXT NOT NULL",
		"installation_id     TEXT NOT NULL",
		"status              TEXT NOT NULL DEFAULT 'pending'",
		"scopes              JSONB NOT NULL",
		"secret_ciphertext   TEXT NOT NULL",
		"UNIQUE (org_id, provider, installation_id)",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("migration missing %q", want)
		}
	}
	router, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router: %v", err)
	}
	body := string(router)
	admin := strings.Index(body, `r.Use(s.requireRole("admin"))`)
	owner := strings.Index(body, `r.Use(s.requireRole("owner"))`)
	if admin < 0 || owner < 0 || admin >= owner {
		t.Fatal("could not locate admin and owner route groups")
	}
	for _, route := range []string{
		`r.Get("/connector-connections", s.handleListConnectorConnections)`,
		`r.Post("/connector-connections", s.handleCreateConnectorConnection)`,
		`r.Get("/connector-connections/{id}", s.handleGetConnectorConnection)`,
		`r.Patch("/connector-connections/{id}", s.handleUpdateConnectorConnection)`,
		`r.Delete("/connector-connections/{id}", s.handleDeleteConnectorConnection)`,
		`r.Post("/connector-connections/{id}/check", s.handleCheckConnectorConnection)`,
		`r.Post("/connector-connections/{id}/sync", s.handleRunConnectorSync)`,
	} {
		at := strings.Index(body, route)
		if at < admin || at >= owner {
			t.Errorf("route %q is not inside admin group", route)
		}
	}
}

func TestConnectorProbeRegistryAllowlistAndOperations(t *testing.T) {
	registry := newConnectorProbeRegistry(nil)
	for _, tc := range []struct {
		provider  string
		operation string
	}{
		{provider: "hubspot", operation: "account_details"},
		{provider: "github", operation: "user"},
		{provider: "slack", operation: "auth.test"},
		{provider: "openai", operation: "models"},
	} {
		adapter, ok := registry.Lookup(tc.provider)
		if !ok || adapter.Operation != tc.operation || adapter.Probe == nil {
			t.Fatalf("%s adapter = %#v/%v, want operation %q", tc.provider, adapter, ok, tc.operation)
		}
	}
	if _, ok := registry.Lookup("https://attacker.example"); ok {
		t.Fatal("registry accepted an arbitrary provider/authority")
	}
}

func stringPtr(s string) *string { return &s }
