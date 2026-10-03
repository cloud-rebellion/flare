package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// TestLogSeverityStats proves the generated aggregate query counts only the
// selected tenant, project, severity, and window when Postgres is available.
func TestLogSeverityStats(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	orgA, projectA := seedProject(t, ctx, q)
	orgB, projectB := seedProject(t, ctx, q)
	now := time.Now().UTC()

	rows := []generated.InsertLogsParams{
		{ID: id.New(), OrgID: orgA, ProjectID: projectA, Severity: "error", Body: "private A", ObservedAt: pgtype.Timestamptz{Time: now, Valid: true}},
		{ID: id.New(), OrgID: orgA, ProjectID: projectA, Severity: "warning", Body: "private warning", ObservedAt: pgtype.Timestamptz{Time: now, Valid: true}},
		{ID: id.New(), OrgID: orgA, ProjectID: projectA, Severity: "warn", Body: "private warn", ObservedAt: pgtype.Timestamptz{Time: now, Valid: true}},
		{ID: id.New(), OrgID: orgA, ProjectID: projectA, Severity: "error", Body: "old", ObservedAt: pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}},
		{ID: id.New(), OrgID: orgB, ProjectID: projectB, Severity: "error", Body: "private B", ObservedAt: pgtype.Timestamptz{Time: now, Valid: true}},
	}
	if _, err := q.InsertLogs(ctx, rows); err != nil {
		t.Fatalf("seed logs: %v", err)
	}
	arg := generated.GetLogSeverityStatsParams{
		ProjectID: projectA, OrgID: orgA, Severity: "error",
		Since: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
	}
	count, err := q.GetLogSeverityStats(ctx, arg)
	if err != nil || count != 1 {
		t.Fatalf("error count = %d, err=%v; want one in-window tenant log", count, err)
	}
	arg.Severity = "warn"
	count, err = q.GetLogSeverityStats(ctx, arg)
	if err != nil || count != 2 {
		t.Fatalf("warn count = %d, err=%v; want canonical and legacy warning", count, err)
	}
	arg.ProjectID, arg.OrgID = projectB, orgB
	count, err = q.GetLogSeverityStats(ctx, arg)
	if err != nil || count != 0 {
		t.Fatalf("other tenant warn count = %d, err=%v; want zero", count, err)
	}
}
