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

func TestConnectorQuotaHealthBounds(t *testing.T) {
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
		r := httptest.NewRequest(http.MethodGet, "/api/connector-quota-health"+tc.query, nil)
		if got := connectorQuotaHealthHours(r); got != tc.hours {
			t.Fatalf("hours(%q) = %d, want %d", tc.query, got, tc.hours)
		}
		if got := connectorQuotaHealthLimit(r); got != tc.limit {
			t.Fatalf("limit(%q) = %d, want %d", tc.query, got, tc.limit)
		}
	}
}

func TestConnectorQuotaHealthMapperIsPayloadFree(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	out := toConnectorQuotaHealthResponse(&generated.ListConnectorQuotaHealthByOrgRow{
		ConnectionID: "conn_hubspot_eu", Provider: "hubspot", Operation: "contacts.list", Channel: "http",
		QuotaPoints: 2, RetryPoints: 1, SyncLagPoints: 1,
		QuotaRemaining:    pgtype.Int8{Int64: 4, Valid: true},
		QuotaObservedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		RetryAfterMs:      pgtype.Int8{Int64: 2500, Valid: true},
		RetryObservedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		SyncLagMs:         pgtype.Int8{Int64: 18000, Valid: true},
		SyncLagObservedAt: pgtype.Timestamptz{Time: now, Valid: true},
		LastCompletedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(body)
	for _, want := range []string{`"connection_id":"conn_hubspot_eu"`, `"provider":"hubspot"`, `"quota_remaining":4`, `"retry_after_ms":2500`, `"sync_lag_ms":18000`, `"quota_points":2`, `"sync_lag_points":1`} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"org_id", "project_id", "url", "token", "payload", "response_secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("quota health response leaked %q: %s", forbidden, text)
		}
	}
}

func TestProjectConnectorQuotaHealthMapperIsPayloadFree(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	out := toProjectConnectorQuotaHealthResponse(&generated.ListConnectorQuotaHealthByProjectRow{
		ConnectionID: "conn_hubspot_eu", Provider: "hubspot", Operation: "contacts.list", Channel: "http",
		QuotaPoints: 1, RetryPoints: 1, SyncLagPoints: 1,
		QuotaRemaining:    pgtype.Int8{Int64: 7, Valid: true},
		QuotaObservedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		RetryAfterMs:      pgtype.Int8{Int64: 1500, Valid: true},
		RetryObservedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		SyncLagMs:         pgtype.Int8{Int64: 9000, Valid: true},
		SyncLagObservedAt: pgtype.Timestamptz{Time: now, Valid: true},
		LastCompletedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(body)
	for _, want := range []string{`"connection_id":"conn_hubspot_eu"`, `"provider":"hubspot"`, `"quota_remaining":7`, `"retry_after_ms":1500`, `"sync_lag_ms":9000`} {
		if !strings.Contains(text, want) {
			t.Fatalf("project response missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"org_id", "project_id", "url", "token", "payload"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("project quota response leaked %q: %s", forbidden, text)
		}
	}
}

func TestConnectorQuotaHealthRouteIsAuthenticatedViewerRead(t *testing.T) {
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
	if !strings.Contains(body[readStart:memberStart], `r.Get("/connector-quota-health", s.handleListConnectorQuotaHealth)`) {
		t.Fatal("connector quota health route is not in authenticated viewer read group")
	}
	if !strings.Contains(body[readStart:memberStart], `r.Get("/projects/{id}/connector-quota-health", s.handleListProjectConnectorQuotaHealth)`) {
		t.Fatal("project connector quota health route is not in authenticated viewer read group")
	}
}
