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

func TestClientHealthBounds(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantHours int
		wantLimit int
	}{
		{name: "defaults", query: "", wantHours: 24, wantLimit: 100},
		{name: "requested", query: "?hours=48&limit=25", wantHours: 48, wantLimit: 25},
		{name: "hours clamp", query: "?hours=9999&limit=9999", wantHours: 720, wantLimit: 100},
		{name: "invalid values", query: "?hours=0&limit=-1", wantHours: 24, wantLimit: 100},
		{name: "malformed values", query: "?hours=wat&limit=wat", wantHours: 24, wantLimit: 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/projects/p/client-health"+tc.query, nil)
			if got := clientHealthHours(r); got != tc.wantHours {
				t.Fatalf("clientHealthHours = %d, want %d", got, tc.wantHours)
			}
			if got := clientHealthLimit(r); got != tc.wantLimit {
				t.Fatalf("clientHealthLimit = %d, want %d", got, tc.wantLimit)
			}
		})
	}
}

func TestClientHealthMapperIsAggregateOnly(t *testing.T) {
	created := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	first := created.Add(time.Minute)
	last := created.Add(2 * time.Minute)
	out := toClientHealthResponse(&generated.ListClientHealthByProjectRow{
		Platform:           "javascript",
		Environment:        "production",
		Release:            "web-2026.10.02",
		EventCount:         4,
		ErrorCount:         2,
		AffectedIssueCount: 2,
		FirstSeen:          pgtype.Timestamptz{Time: first, Valid: true},
		LastSeen:           pgtype.Timestamptz{Time: last, Valid: true},
	})
	if out.ErrorRate != 0.5 {
		t.Fatalf("error rate = %v, want 0.5", out.ErrorRate)
	}
	if out.FirstSeen == nil || !out.FirstSeen.Equal(first) || out.LastSeen == nil || !out.LastSeen.Equal(last) {
		t.Fatalf("timestamps = %#v, want first/last", out)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(body)
	for _, want := range []string{`"platform":"javascript"`, `"environment":"production"`, `"release":"web-2026.10.02"`, `"event_count":4`, `"error_count":2`, `"error_rate":0.5`, `"affected_issue_count":2`} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"org_id", "project_id", "payload", "stacktrace", "message", "labels"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("aggregate response leaked %q: %s", forbidden, text)
		}
	}

	zero := toClientHealthResponse(&generated.ListClientHealthByProjectRow{Platform: "flutter"})
	if zero.ErrorRate != 0 || zero.FirstSeen != nil || zero.LastSeen != nil {
		t.Fatalf("zero-volume client group mapped unexpectedly: %#v", zero)
	}
}

func TestClientHealthRouteIsAuthenticatedViewerRead(t *testing.T) {
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
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/client-health", s.handleListClientHealth)`) {
		t.Fatal("client health route is not in the authenticated viewer read group")
	}
}

func TestClientHealthQueryScopesBothTenantAndProject(t *testing.T) {
	src, err := os.ReadFile("../db/queries/client_health.sql")
	if err != nil {
		t.Fatalf("read client_health.sql: %v", err)
	}
	body := string(src)
	for _, required := range []string{
		"e.project_id = sqlc.arg(project_id)",
		"e.org_id = sqlc.arg(org_id)",
		"e.received_at >= sqlc.arg(since)",
		"GROUP BY e.platform, e.environment, e.release",
		"LIMIT sqlc.arg(row_limit)",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("client health query missing %q", required)
		}
	}
	for _, forbidden := range []string{"e.payload", "e.stacktrace", "e.attributes", "e.labels"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("client health query selects raw %q", forbidden)
		}
	}
}
