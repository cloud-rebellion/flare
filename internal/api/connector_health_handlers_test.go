package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestConnectorHealthBounds(t *testing.T) {
	for _, tc := range []struct {
		query string
		hours int
		limit int
	}{
		{"", 24, 100},
		{"?hours=48&limit=25", 48, 25},
		{"?hours=9999&limit=9999", 720, 100},
		{"?hours=0&limit=-1", 24, 100},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/connector-health"+tc.query, nil)
		if got := connectorHealthHours(r); got != tc.hours {
			t.Fatalf("hours(%q) = %d, want %d", tc.query, got, tc.hours)
		}
		if got := connectorHealthLimit(r); got != tc.limit {
			t.Fatalf("limit(%q) = %d, want %d", tc.query, got, tc.limit)
		}
	}
}

func TestConnectorHealthMapperIsPayloadFree(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	out := toConnectorHealthResponse(&generated.ListConnectorHealthByOrgRow{
		ConnectionID: "conn-prod",
		Provider:     "alerts", Operation: "dispatch", Channel: "slack",
		Attempts: 4, OkCount: 3, ErrorCount: 1, AvgElapsedMs: 12.5, MaxElapsedMs: 33,
		LastAttemptedAt: pgtype.Timestamptz{Time: now, Valid: true},
		LastCompletedAt: pgtype.Timestamptz{Time: now.Add(time.Second), Valid: true},
	})
	if out.ErrorRate != 0.25 {
		t.Fatalf("error rate = %v, want 0.25", out.ErrorRate)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(body)
	for _, want := range []string{`"connection_id":"conn-prod"`, `"provider":"alerts"`, `"operation":"dispatch"`, `"error_rate":0.25`, `"max_elapsed_ms":33`} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"org_id", "project_id", "url", "token", "payload", "response_secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("health response leaked %q: %s", forbidden, text)
		}
	}
}

func TestConnectorHealthRouteIsAuthenticatedViewerRead(t *testing.T) {
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
	if !strings.Contains(body[readStart:memberStart], `r.Get("/connector-health", s.handleListConnectorHealth)`) {
		t.Fatal("connector health route is not in authenticated viewer read group")
	}
	if !strings.Contains(body[readStart:memberStart], `r.Get("/projects/{id}/connector-health", s.handleListProjectConnectorHealth)`) {
		t.Fatal("project connector health route is not in authenticated viewer read group")
	}
}
