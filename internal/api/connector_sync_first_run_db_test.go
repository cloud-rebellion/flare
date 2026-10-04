package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/jackc/pgx/v5/pgtype"
)

// A project-bound installation must become overdue after its first-run grace
// even if neither the opt-in scheduler nor an administrator has created a sync
// state. Run against a migrated PostgreSQL database with FLARE_TEST_DATABASE_URL.
func TestConnectorSyncHealthFirstRunGraceAndScopeDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	q := generated.New(pool)
	owner := seedProjectForIngest(t, pool)
	foreignOrg := seedProjectForIngest(t, pool)
	foreignProject, err := q.CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: owner.OrgID, Name: "other", Slug: "other-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := q.CreateConnectorConnection(ctx, generated.CreateConnectorConnectionParams{
		ID: id.New(), OrgID: owner.OrgID, ProjectID: pgtype.Text{String: owner.ID, Valid: true},
		Provider: "hubspot", InstallationID: "first-run-test", Status: "active",
		Scopes: json.RawMessage(`[]`), SecretCiphertext: "enc:v1:opaque",
	})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(-5 * time.Minute)
	stats := func(orgID, projectID, provider string) *generated.GetConnectorSyncHealthStatsRow {
		t.Helper()
		row, err := q.GetConnectorSyncHealthStats(ctx, generated.GetConnectorSyncHealthStatsParams{
			OrgID: orgID, ProjectID: projectID, ConnectionID: connection.ID,
			Provider: provider, Operation: "account_details",
			Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	assertOverdue := func(want int64) {
		t.Helper()
		if got := stats(owner.OrgID, owner.ID, "hubspot"); got.OverdueCount != want || got.FailedCount != 0 {
			t.Fatalf("owner sync health = %#v, want overdue=%d failed=0", got, want)
		}
	}
	ruleID := id.New()
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules
		(id, project_id, org_id, name, type, threshold, window_minutes,
		 connector_provider, connector_operation, connector_connection_id, connector_sync_mode)
		VALUES ($1,$2,$3,'First HubSpot check overdue','connector_sync_health',1,5,
		        'hubspot','account_details',$4,'overdue')`, ruleID, owner.ID, owner.OrgID, connection.ID); err != nil {
		t.Fatal(err)
	}
	watchdogReason := func() string {
		t.Helper()
		rules, err := q.ListWatchdogRules(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range rules {
			if rule.ID == ruleID {
				return serverForDB(pool).watchdogReason(ctx, rule, time.Now().UTC())
			}
		}
		t.Fatal("connector sync alert rule missing")
		return ""
	}

	assertOverdue(0) // First run still has its five-minute grace.
	if reason := watchdogReason(); reason != "" {
		t.Fatalf("first-run alert fired during grace: %q", reason)
	}
	if _, err := pool.Exec(ctx, `UPDATE connector_connections SET created_at=$2 WHERE id=$1`, connection.ID, cutoff.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertOverdue(1) // No state row exists, but the first pass is now overdue.
	if reason := watchdogReason(); reason == "" {
		t.Fatal("first-run alert did not fire after grace")
	}
	for _, scope := range []struct{ orgID, projectID, provider string }{
		{owner.OrgID, foreignProject.ID, "hubspot"},
		{foreignOrg.OrgID, owner.ID, "hubspot"},
		{owner.OrgID, owner.ID, "github"},
	} {
		if got := stats(scope.orgID, scope.projectID, scope.provider); got.OverdueCount != 0 || got.FailedCount != 0 {
			t.Fatalf("foreign scope %#v observed %#v", scope, got)
		}
	}
	for _, status := range []string{"paused", "revoked", "pending"} {
		if _, err := pool.Exec(ctx, `UPDATE connector_connections SET status=$2 WHERE id=$1`, connection.ID, status); err != nil {
			t.Fatal(err)
		}
		assertOverdue(0)
		if reason := watchdogReason(); reason != "" {
			t.Fatalf("alert fired for %s connection: %q", status, reason)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE connector_connections SET status='active' WHERE id=$1`, connection.ID); err != nil {
		t.Fatal(err)
	}

	// An idle row reset by re-consent starts a new grace period. The previous
	// attempt timestamp survives that reset, so it must not hide an overdue row.
	if _, err := q.EnsureConnectorSyncState(ctx, generated.EnsureConnectorSyncStateParams{
		ID: id.New(), OrgID: owner.OrgID, ConnectionID: connection.ID, Operation: "account_details",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE connector_sync_states SET last_started_at=$2 WHERE connection_id=$1`, connection.ID, cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertOverdue(0)
	if _, err := pool.Exec(ctx, `UPDATE connector_sync_states SET updated_at=$2 WHERE connection_id=$1`, connection.ID, cutoff.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertOverdue(1)
	workerID := "first-run-test-worker"
	if _, err := q.TryClaimConnectorSync(ctx, generated.TryClaimConnectorSyncParams{
		OrgID: owner.OrgID, ConnectionID: connection.ID, Operation: "account_details", LeaseOwner: workerID,
		LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	assertOverdue(0) // A live worker lease is not overdue.
	if _, err := pool.Exec(ctx, `UPDATE connector_sync_states SET lease_expires_at=$2 WHERE connection_id=$1`, connection.ID, cutoff.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertOverdue(1) // An expired lease remains covered.
	if _, err := q.CompleteConnectorSync(ctx, generated.CompleteConnectorSyncParams{
		OrgID: owner.OrgID, ConnectionID: connection.ID, Operation: "account_details", LeaseOwner: workerID,
		Outcome: "ok", NextRunAt: pgtype.Timestamptz{Time: cutoff.Add(-time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	assertOverdue(1) // A completed pass with a missed next run remains covered.
	if _, err := pool.Exec(ctx, `UPDATE connector_sync_states SET next_run_at=$2 WHERE connection_id=$1`, connection.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertOverdue(0)
}
