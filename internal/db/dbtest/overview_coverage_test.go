package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/id"
)

func TestOverviewCoverageUsesCompletedRecentTenantReceipts(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	otherOrg, otherProject := seedProject(t, ctx, q)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM orgs WHERE id IN ($1, $2)", org, otherOrg)
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	acceptedAt := now.Add(-2 * time.Hour)
	for _, row := range []struct {
		org, project, pillar        string
		accepted, rejected, dropped int64
		received                    time.Time
		completed                   bool
	}{
		{org, project, "logs", 2, 1, 1, acceptedAt, true},
		{org, project, "logs", 0, 5, 0, now.Add(-time.Hour), true},
		{org, project, "metrics", 99, 0, 0, now.Add(-25 * time.Hour), true},
		{org, project, "spans", 77, 0, 0, now.Add(-time.Hour), false},
		{otherOrg, otherProject, "logs", 123, 0, 0, now.Add(-time.Hour), true},
	} {
		var completed any
		if row.completed {
			completed = row.received.Add(time.Second)
		}
		_, err := pool.Exec(ctx, `INSERT INTO ingest_receipts
			(id, org_id, project_id, pillar, protocol, received_count, accepted_count,
			 rejected_count, dropped_count, persisted_count, outcome, http_status, received_at, completed_at)
			VALUES ($1,$2,$3,$4,'native',$5,$6,$7,$8,$6,'accepted',200,$9,$10)`,
			id.New(), row.org, row.project, row.pillar, row.accepted+row.rejected+row.dropped,
			row.accepted, row.rejected, row.dropped, row.received, completed)
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := q.OverviewIngestCoverage24h(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("coverage rows = %+v; want only this tenant's completed recent logs", rows)
	}
	got := rows[0]
	if got.ProjectID != project || got.Pillar != "logs" || got.AcceptedCount != 2 || got.RejectedCount != 6 || got.DroppedCount != 1 {
		t.Fatalf("coverage included another tenant, expired or pending receipt: %+v", got)
	}
	if !got.LastAcceptedAt.Valid || !got.LastAcceptedAt.Time.Equal(acceptedAt) {
		t.Fatalf("newer rejected request advanced last accepted time: %+v", got.LastAcceptedAt)
	}
}
