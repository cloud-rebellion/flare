package dbtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// This test is skipped without TEST_DATABASE_URL, like the other dbtest
// package tests. With Postgres it proves connection state, cursor versioning,
// lease ownership, and organization predicates all hold in the real schema.
func TestConnectorSyncStateIsTenantScopedAndLeaseGuarded(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	orgA, projectA := seedProject(t, ctx, q)
	orgB, _ := seedProject(t, ctx, q)

	connection, err := q.CreateConnectorConnection(ctx, generated.CreateConnectorConnectionParams{
		ID: id.New(), OrgID: orgA, Provider: "hubspot", InstallationID: "install-sync-1",
		InstallationName: "Production", Status: "active", Scopes: json.RawMessage(`[]`),
		SecretCiphertext: "enc:v1:opaque",
	})
	if err != nil {
		t.Fatalf("create connector connection: %v", err)
	}
	if _, err := q.EnsureConnectorSyncState(ctx, generated.EnsureConnectorSyncStateParams{
		ID: id.New(), OrgID: orgA, ConnectionID: connection.ID, Operation: "account_details",
	}); err != nil {
		t.Fatalf("ensure sync state: %v", err)
	}
	if _, err := q.GetConnectorSyncState(ctx, generated.GetConnectorSyncStateParams{
		OrgID: orgB, ConnectionID: connection.ID, Operation: "account_details",
	}); err == nil {
		t.Fatal("cross-tenant sync-state read returned a row")
	}
	if _, err := q.ListConnectorSyncHealthByOrg(ctx, generated.ListConnectorSyncHealthByOrgParams{OrgID: orgB, Limit: 10}); err != nil {
		t.Fatalf("cross-tenant sync health query: %v", err)
	}

	leaseUntil := pgtype.Timestamptz{Time: time.Now().UTC().Add(5 * time.Minute), Valid: true}
	claimed, err := q.TryClaimConnectorSync(ctx, generated.TryClaimConnectorSyncParams{
		OrgID: orgA, ConnectionID: connection.ID, Operation: "account_details", LeaseOwner: "worker-a", LeaseExpiresAt: leaseUntil,
	})
	if err != nil {
		t.Fatalf("claim sync state: %v", err)
	}
	if claimed.State != "running" || claimed.AttemptCount != 1 || !claimed.LeaseActive {
		t.Fatalf("claim projection = %#v", claimed)
	}
	if _, err := q.TryClaimConnectorSync(ctx, generated.TryClaimConnectorSyncParams{
		OrgID: orgA, ConnectionID: connection.ID, Operation: "account_details", LeaseOwner: "worker-b", LeaseExpiresAt: leaseUntil,
	}); err == nil {
		t.Fatal("second worker claimed an unexpired lease")
	} else if err != pgx.ErrNoRows {
		t.Fatalf("second worker claim = %v, want no rows", err)
	}

	next := pgtype.Timestamptz{Time: time.Now().UTC().Add(time.Hour), Valid: true}
	completed, err := q.CompleteConnectorSync(ctx, generated.CompleteConnectorSyncParams{
		OrgID: orgA, ConnectionID: connection.ID, Operation: "account_details", LeaseOwner: "worker-a",
		Outcome: "ok", ReplaceCursor: true, CursorCiphertext: "enc:v1:cursor",
		LastErrorCode: "", LastSyncLagMs: pgtype.Int8{Int64: 250, Valid: true}, NextRunAt: next,
	})
	if err != nil {
		t.Fatalf("complete sync state: %v", err)
	}
	if completed.State != "succeeded" || completed.CursorVersion != 1 || !completed.CursorConfigured || completed.LeaseActive {
		t.Fatalf("completion projection = %#v", completed)
	}
	cursor, err := q.GetConnectorSyncCursor(ctx, generated.GetConnectorSyncCursorParams{OrgID: orgA, ConnectionID: connection.ID, Operation: "account_details"})
	if err != nil || cursor.CursorCiphertext != "enc:v1:cursor" || cursor.CursorVersion != 1 {
		t.Fatalf("cursor = %#v/%v", cursor, err)
	}
	if completed.LastSyncLagMs.Int64 != 250 || !completed.LastSyncLagMs.Valid {
		t.Fatalf("sync lag = %#v", completed.LastSyncLagMs)
	}

	if _, err := q.CompleteConnectorSync(ctx, generated.CompleteConnectorSyncParams{
		OrgID: orgA, ConnectionID: connection.ID, Operation: "account_details", LeaseOwner: "worker-b",
		Outcome: "ok", ReplaceCursor: false, NextRunAt: next,
	}); err == nil {
		t.Fatal("non-owner completed sync state")
	} else if err != pgx.ErrNoRows {
		t.Fatalf("non-owner completion = %v, want no rows", err)
	}
	_ = projectA // keep the seeded project alive for the org fixture.
}
