package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/automationoutbox"
	"github.com/bright-interaction/flare/internal/config"
)

func TestWorkerSnapshotReportsBoundedOccupancy(t *testing.T) {
	ch := make(chan struct{}, 4)
	ch <- struct{}{}
	ch <- struct{}{}
	got := workerSnapshot(ch)
	if got.Capacity != 4 || got.Busy != 2 || got.Available != 2 {
		t.Fatalf("worker snapshot = %#v, want capacity=4 busy=2 available=2", got)
	}
	if got := workerSnapshot(nil); got != (healthWorkerSnapshot{}) {
		t.Fatalf("nil worker snapshot = %#v, want zero", got)
	}
}

func TestRetentionHealthPolicies(t *testing.T) {
	for _, tc := range []struct {
		name          string
		allowDrop     bool
		analytics     bool
		wantPolicy    string
		wantExportReq bool
	}{
		{name: "cold tier available", analytics: true, wantPolicy: "export_before_drop", wantExportReq: true},
		{name: "cold tier unavailable", wantPolicy: "paused", wantExportReq: true},
		{name: "explicit opt out", allowDrop: true, wantPolicy: "drop_without_export", wantExportReq: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, exportRequired := retentionHealth(tc.allowDrop, tc.analytics)
			if policy != tc.wantPolicy || exportRequired != tc.wantExportReq {
				t.Fatalf("retentionHealth = %q/%t, want %q/%t", policy, exportRequired, tc.wantPolicy, tc.wantExportReq)
			}
		})
	}
}

func TestReadinessStatusSeparatesCoreAndOptionalFailures(t *testing.T) {
	for _, tc := range []struct {
		name                string
		dbOK                bool
		analyticsConfigured bool
		analyticsOK         bool
		policy              string
		wantStatus          string
		wantCode            int
	}{
		{name: "database unavailable", wantStatus: "degraded", wantCode: http.StatusServiceUnavailable},
		{name: "all core and optional dependencies healthy", dbOK: true, analyticsConfigured: true, analyticsOK: true, policy: "export_before_drop", wantStatus: "ok", wantCode: http.StatusOK},
		{name: "optional analytics degraded", dbOK: true, analyticsConfigured: true, policy: "export_before_drop", wantStatus: "degraded", wantCode: http.StatusOK},
		{name: "retention paused", dbOK: true, policy: "paused", wantStatus: "degraded", wantCode: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, code := readinessStatus(tc.dbOK, tc.analyticsConfigured, tc.analyticsOK, tc.policy)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("readinessStatus = %q/%d, want %q/%d", status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestHandleReadyDatabaseUnavailableReturns503AndBoundedJSON(t *testing.T) {
	sha := strings.Repeat("a", 40)
	srv := &Server{
		cfg:              config.Config{RetentionDays: 14, Release: "v0.4.0+g" + sha, SourceCommit: sha},
		automationOutbox: automationoutbox.New(nil),
	}
	rec := httptest.NewRecorder()
	srv.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var got healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if got.Status != "degraded" || got.DB.Healthy {
		t.Fatalf("readiness response = %#v, want degraded/unhealthy DB", got)
	}
	if got.Release != "v0.4.0+g"+sha || got.SourceCommit != sha {
		t.Fatalf("readiness identity = %q / %q, want exact stamped release and source commit", got.Release, got.SourceCommit)
	}
	if got.Retention.Policy != "paused" || got.Retention.Days != 14 {
		t.Fatalf("retention response = %#v, want paused/14", got.Retention)
	}
	if got.ConnectorReceipts.State != "unobserved" || got.ConnectorReceipts.Durable || got.ConnectorReceipts.Started != 0 {
		t.Fatalf("connector receipt projection = %#v, want unobserved and non-durable", got.ConnectorReceipts)
	}
	if got.Queue.Mode != "bounded_drop" || got.Queue.Queued != 0 || got.Queue.Durable || !got.Queue.DropOnFull {
		t.Fatalf("queue response = %#v, want explicit bounded drop mode", got.Queue)
	}
	if got.AutomationOutbox.State != "unavailable" {
		t.Fatalf("automation outbox = %#v, want unavailable with database down", got.AutomationOutbox)
	}
	if got.IssueEvaluations.State != "unavailable" {
		t.Fatalf("issue evaluations = %#v, want unavailable with database down", got.IssueEvaluations)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"org_id", "project_id", "token", "payload", "secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("readiness response leaked %q: %s", forbidden, body)
		}
	}
}

func TestHealthRoutesAreUnauthenticatedRootProbes(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	body := string(src)
	apiStart := strings.Index(body, `r.Route("/api"`)
	if apiStart < 0 {
		t.Fatal("router API group missing")
	}
	for _, route := range []string{`r.Get("/livez", s.handleLive)`, `r.Get("/readyz", s.handleReady)`, `r.Get("/healthz", s.handleReady)`, `r.Get("/health", s.handleHealth)`} {
		at := strings.Index(body, route)
		if at < 0 {
			t.Fatalf("missing route %s", route)
		}
		if route == `r.Get("/health", s.handleHealth)` && at < apiStart {
			t.Fatalf("API health alias registered outside /api group")
		}
		if route != `r.Get("/health", s.handleHealth)` && at > apiStart {
			t.Fatalf("root probe %s registered inside authenticated API group", route)
		}
	}
}
