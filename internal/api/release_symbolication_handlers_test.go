package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestReleaseSymbolicationHealthMapperCalculatesCoverage(t *testing.T) {
	created := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	first := created.Add(time.Minute)
	last := created.Add(2 * time.Minute)
	out := toReleaseSymbolicationHealthResponse(&generated.ListReleaseSymbolicationHealthByProjectRow{
		Version:              "web-2026.10.02",
		CreatedAt:            pgtype.Timestamptz{Time: created, Valid: true},
		MapCount:             2,
		MapBytes:             4096,
		EventCount:           4,
		StacktraceEventCount: 3,
		CandidateFrameCount:  5,
		MappedFrameCount:     4,
		UnmappedFrameCount:   1,
		FirstSeen:            pgtype.Timestamptz{Time: first, Valid: true},
		LastSeen:             pgtype.Timestamptz{Time: last, Valid: true},
	})
	if out.CoverageRate == nil || *out.CoverageRate != 0.8 {
		t.Fatalf("coverage rate = %v, want 0.8", out.CoverageRate)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(body)
	for _, want := range []string{`"version":"web-2026.10.02"`, `"map_count":2`, `"map_bytes":4096`, `"candidate_frame_count":5`, `"mapped_frame_count":4`, `"unmapped_frame_count":1`, `"coverage_rate":0.8`} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"content", `"stacktrace":`, `"payload":`, "org_id", "project_id"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("aggregate response leaked %q: %s", forbidden, text)
		}
	}

	zero := toReleaseSymbolicationHealthResponse(&generated.ListReleaseSymbolicationHealthByProjectRow{Version: "no-frames"})
	if zero.CoverageRate != nil {
		t.Fatalf("zero-frame release has coverage rate %v, want nil", *zero.CoverageRate)
	}
}

func TestReleaseSymbolicationHealthRouteIsAuthenticatedViewerRead(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	body := string(src)
	readStart := strings.Index(body, "r.Use(s.requireAuth)")
	memberStart := strings.Index(body, `s.requireRole("member")`)
	if readStart < 0 || memberStart < 0 || readStart >= memberStart {
		t.Fatal("router auth/read groups changed")
	}
	readBlock := body[readStart:memberStart]
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/symbolication-health", s.handleListReleaseSymbolicationHealth)`) {
		t.Fatal("symbolication health route is not in the authenticated viewer read group")
	}
}

func TestReleaseSymbolicationHealthBoundsReuseReleaseHealthWindow(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/projects/p/symbolication-health?hours=9999&limit=9999", nil)
	if got := releaseHealthHours(r); got != maxReleaseHealthHours {
		t.Fatalf("hours = %d, want %d", got, maxReleaseHealthHours)
	}
	if got := releaseHealthLimit(r); got != maxReleaseHealthLimit {
		t.Fatalf("limit = %d, want %d", got, maxReleaseHealthLimit)
	}
}

func TestReleaseSymbolicationHealthQueryIsTenantScopedAndPayloadFree(t *testing.T) {
	src, err := os.ReadFile("../db/queries/releases.sql")
	if err != nil {
		t.Fatalf("read releases.sql: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "-- name: ListReleaseSymbolicationHealthByProject")
	if start < 0 {
		t.Fatal("symbolication health query missing")
	}
	query := body[start:]
	for _, required := range []string{
		"r.project_id = sqlc.arg(project_id)",
		"r.org_id = sqlc.arg(org_id)",
		"e.project_id = sqlc.arg(project_id)",
		"e.org_id = sqlc.arg(org_id)",
		"sm.project_id = sqlc.arg(project_id)",
		"sm.org_id = sqlc.arg(org_id)",
		"jsonb_array_elements",
		"frame.value ->> 'lineno' ~ '^[1-9][0-9]*$'",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("symbolication health query missing %q", required)
		}
	}
	if strings.Contains(query, "SELECT sm.content") || strings.Contains(query, "SELECT e.payload") {
		t.Fatal("symbolication health query selects raw map or event payload")
	}
}
