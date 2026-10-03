package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func TestConnectorQuotaStatsAreTenantProjectScopedAndLatest(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	orgA, projectA := seedProject(t, ctx, q)
	orgB, projectB := seedProject(t, ctx, q)
	now := time.Now().UTC()
	insert := func(org, project string, at time.Time, quota int64) {
		t.Helper()
		if err := q.InsertConnectorAttempt(ctx, generated.InsertConnectorAttemptParams{
			ID: id.New(), OrgID: org, ProjectID: pgtype.Text{String: project, Valid: true},
			Provider: "hubspot", Operation: "contacts.list", Channel: "http", Outcome: "ok",
			StatusClass: "2xx", ElapsedMs: 20,
			QuotaRemaining: pgtype.Int8{Int64: quota, Valid: true},
			SyncLagMs:      pgtype.Int8{Int64: 1000 + quota, Valid: true},
			AttemptedAt:    pgtype.Timestamptz{Time: at, Valid: true},
			CompletedAt:    pgtype.Timestamptz{Time: at, Valid: true},
		}); err != nil {
			t.Fatalf("insert connector attempt: %v", err)
		}
	}
	insert(orgA, projectA, now.Add(-10*time.Minute), 100)
	insert(orgA, projectA, now.Add(-time.Minute), 4)
	insert(orgB, projectB, now.Add(-time.Minute), 0)

	arg := generated.GetConnectorQuotaStatsParams{
		Metric: "quota_remaining", ProjectID: projectA, OrgID: orgA,
		Provider: "hubspot", Operation: "contacts.list", Channel: "http",
		CompletedAt: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
	}
	got, err := q.GetConnectorQuotaStats(ctx, arg)
	if err != nil {
		t.Fatalf("quota stats: %v", err)
	}
	if got.Points != 2 || got.LatestValue != 4 {
		t.Fatalf("quota stats = %#v, want 2 points/latest 4", got)
	}
	arg.Metric = "sync_lag_ms"
	gotLag, err := q.GetConnectorQuotaStats(ctx, arg)
	if err != nil {
		t.Fatalf("sync lag stats: %v", err)
	}
	if gotLag.Points != 2 || gotLag.LatestValue != 1004 {
		t.Fatalf("sync lag stats = %#v, want 2 points/latest 1004", gotLag)
	}
	arg.Metric = "quota_remaining"

	// A project id from one tenant paired with another tenant must not see the
	// row, even when every provider selector matches.
	arg.OrgID = orgB
	wrongTenant, err := q.GetConnectorQuotaStats(ctx, arg)
	if err != nil {
		t.Fatalf("wrong tenant quota stats: %v", err)
	}
	if wrongTenant.Points != 0 || wrongTenant.LatestValue != 0 {
		t.Fatalf("cross-tenant quota stats = %#v, want empty", wrongTenant)
	}

	arg.ProjectID, arg.OrgID = projectB, orgB
	other, err := q.GetConnectorQuotaStats(ctx, arg)
	if err != nil {
		t.Fatalf("other project quota stats: %v", err)
	}
	if other.Points != 1 || other.LatestValue != 0 {
		t.Fatalf("other project quota stats = %#v, want one point/latest 0", other)
	}
}
