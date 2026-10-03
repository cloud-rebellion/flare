package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// TestReleaseErrorRateStats exercises the real migration and generated query
// when TEST_DATABASE_URL is available. It proves that an empty selector picks
// the newest recent release, an explicit selector stays project/tenant scoped,
// and only aggregate counts leave the database.
func TestReleaseErrorRateStats(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	oldVersion, newVersion := "web-old", "web-new"
	for _, version := range []string{oldVersion, newVersion} {
		if err := q.UpsertRelease(ctx, generated.UpsertReleaseParams{ID: id.New(), ProjectID: project, OrgID: org, Version: version}); err != nil {
			t.Fatalf("upsert release %q: %v", version, err)
		}
	}
	for _, event := range []struct {
		version string
		level   string
	}{
		{oldVersion, "error"},
		{oldVersion, "info"},
		{newVersion, "error"},
		{newVersion, "info"},
	} {
		if err := q.InsertEvent(ctx, generated.InsertEventParams{
			ID: id.New(), ProjectID: project, OrgID: org,
			Level: event.level, Release: event.version,
			Stacktrace: []byte(`{}`), Payload: []byte(`{}`),
			TraceID: pgtype.Text{}, SpanID: pgtype.Text{},
		}); err != nil {
			t.Fatalf("insert %s event: %v", event.version, err)
		}
	}

	arg := generated.GetReleaseErrorRateStatsParams{
		ProjectID: project, OrgID: org,
		Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}
	latest, err := q.GetReleaseErrorRateStats(ctx, arg)
	if err != nil {
		t.Fatalf("latest release stats: %v", err)
	}
	if latest.ReleaseVersion != newVersion || latest.EventCount != 2 || latest.ErrorCount != 1 {
		t.Fatalf("latest release stats = %#v, want %s with 2 events/1 error", latest, newVersion)
	}

	arg.ReleaseVersion = oldVersion
	explicit, err := q.GetReleaseErrorRateStats(ctx, arg)
	if err != nil {
		t.Fatalf("explicit release stats: %v", err)
	}
	if explicit.ReleaseVersion != oldVersion || explicit.EventCount != 2 || explicit.ErrorCount != 1 {
		t.Fatalf("explicit release stats = %#v, want %s with 2 events/1 error", explicit, oldVersion)
	}
}
