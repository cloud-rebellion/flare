package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// Trace/span IDs are client supplied. Their uniqueness must be scoped to a
// project even when two projects report the exact same start timestamp.
func TestSpanKeyAndTraceReadsAreProjectScoped(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	orgA, projectA := seedProject(t, ctx, q)
	orgB, projectB := seedProject(t, ctx, q)
	projectC, err := q.CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: orgA, Name: "second project", Slug: id.New(),
		Platform: "go", PublicKey: id.New(), DsnID: id.New()[:12],
	})
	if err != nil {
		t.Fatalf("create second project in org A: %v", err)
	}

	traceID, spanID := id.New(), id.New()
	start := time.Now().UTC().Truncate(time.Microsecond)
	cases := []struct {
		org, project, name string
	}{
		{orgA, projectA, "org-A-project-A"},
		{orgB, projectB, "org-B-project-B"},
		{orgA, projectC.ID, "org-A-project-C"},
	}
	for _, tc := range cases {
		_, err := q.InsertSpans(ctx, []generated.InsertSpansParams{{
			TraceID: traceID, SpanID: spanID, OrgID: tc.org, ProjectID: tc.project,
			Name: tc.name, StartTime: pgtype.Timestamptz{Time: start, Valid: true},
			EndTime:    pgtype.Timestamptz{Time: start.Add(time.Millisecond), Valid: true},
			Attributes: []byte(`{}`),
		}})
		if err != nil {
			t.Fatalf("copy span for %s: %v", tc.name, err)
		}
	}
	// Read only after every colliding tuple has been stored, so leakage from a
	// later project cannot pass by arriving after the first project's read.
	for _, tc := range cases {
		spans, err := q.GetTraceSpans(ctx, generated.GetTraceSpansParams{
			TraceID: traceID, ProjectID: tc.project, OrgID: tc.org, Limit: 10,
		})
		if err != nil || len(spans) != 1 || spans[0].Name != tc.name {
			t.Fatalf("project-scoped trace for %s: spans=%+v err=%v", tc.name, spans, err)
		}
		summaries, err := q.ListTraces(ctx, generated.ListTracesParams{
			ProjectID: tc.project, OrgID: tc.org, Limit: 10,
			Since: pgtype.Timestamptz{Time: start.Add(-time.Second), Valid: true},
		})
		if err != nil || len(summaries) != 1 || summaries[0].RootName != tc.name || summaries[0].SpanCount != 1 {
			t.Fatalf("project-scoped summary for %s: traces=%+v err=%v", tc.name, summaries, err)
		}
	}

	wrongOrg, err := q.GetTraceSpans(ctx, generated.GetTraceSpansParams{
		TraceID: traceID, ProjectID: projectA, OrgID: orgB, Limit: 10,
	})
	if err != nil || len(wrongOrg) != 0 {
		t.Fatalf("project A trace exposed through org B: spans=%+v err=%v", wrongOrg, err)
	}
	// The project boundary permits the three rows above, but still rejects a
	// second copy of one project's exact tuple.
	_, err = q.InsertSpans(ctx, []generated.InsertSpansParams{{
		TraceID: traceID, SpanID: spanID, OrgID: orgA, ProjectID: projectA,
		Name: "duplicate", StartTime: pgtype.Timestamptz{Time: start, Valid: true},
		EndTime: pgtype.Timestamptz{Time: start.Add(time.Millisecond), Valid: true},
	}})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("same-project duplicate was not rejected by the primary key: %v", err)
	}
}
