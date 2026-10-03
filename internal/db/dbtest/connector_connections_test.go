package dbtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// This test is skipped without TEST_DATABASE_URL, like the other dbtest
// package tests. When Postgres is available it proves the registry's list,
// secret, update, and delete queries remain tenant-scoped in the real schema.
func TestConnectorConnectionQueriesAreTenantScoped(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	orgA, _ := seedProject(t, ctx, q)
	orgB, _ := seedProject(t, ctx, q)

	created, err := q.CreateConnectorConnection(ctx, generated.CreateConnectorConnectionParams{
		ID: id.New(), OrgID: orgA, Provider: "hubspot", InstallationID: "install-1",
		InstallationName: "Production", Status: "active", Scopes: json.RawMessage(`["crm.read"]`),
		SecretCiphertext: "enc:v1:opaque",
	})
	if err != nil {
		t.Fatalf("create connector connection: %v", err)
	}
	if !created.CredentialConfigured || created.OrgID != orgA {
		t.Fatalf("created projection = %#v", created)
	}

	rows, err := q.ListConnectorConnectionsByOrg(ctx, orgA)
	if err != nil {
		t.Fatalf("list connector connections: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != created.ID || !rows[0].CredentialConfigured {
		t.Fatalf("org A list = %#v", rows)
	}
	other, err := q.ListConnectorConnectionsByOrg(ctx, orgB)
	if err != nil {
		t.Fatalf("list other tenant connector connections: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-tenant list returned %#v", other)
	}

	if _, err := q.GetConnectorConnectionByID(ctx, generated.GetConnectorConnectionByIDParams{ID: created.ID, OrgID: orgB}); err == nil {
		t.Fatal("cross-tenant get returned a row")
	}
	secret, err := q.GetConnectorConnectionSecret(ctx, generated.GetConnectorConnectionSecretParams{ID: created.ID, OrgID: orgA})
	if err != nil || secret != "enc:v1:opaque" {
		t.Fatalf("secret query = %q/%v", secret, err)
	}

	updated, err := q.UpdateConnectorConnection(ctx, generated.UpdateConnectorConnectionParams{
		ID: created.ID, OrgID: orgA, Provider: "hubspot", InstallationID: "install-1",
		InstallationName: "Production EU", Status: "paused", Scopes: json.RawMessage(`["crm.read","crm.write"]`),
		SecretCiphertext: secret,
	})
	if err != nil {
		t.Fatalf("update connector connection: %v", err)
	}
	if updated.InstallationName != "Production EU" || updated.Status != "paused" || !updated.CredentialConfigured {
		t.Fatalf("updated projection = %#v", updated)
	}
	secretAfter, err := q.GetConnectorConnectionSecret(ctx, generated.GetConnectorConnectionSecretParams{ID: created.ID, OrgID: orgA})
	if err != nil || secretAfter != secret {
		t.Fatalf("secret changed during metadata update = %q/%v", secretAfter, err)
	}
	if deleted, err := q.DeleteConnectorConnection(ctx, generated.DeleteConnectorConnectionParams{ID: created.ID, OrgID: orgB}); err != nil || deleted != 0 {
		t.Fatalf("cross-tenant delete = %d/%v", deleted, err)
	}
	if deleted, err := q.DeleteConnectorConnection(ctx, generated.DeleteConnectorConnectionParams{ID: created.ID, OrgID: orgA}); err != nil || deleted != 1 {
		t.Fatalf("owner delete = %d/%v", deleted, err)
	}
}
