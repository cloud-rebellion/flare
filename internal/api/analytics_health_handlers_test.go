package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/analytics"
	"github.com/bright-interaction/flare/internal/config"
	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestExportConnectionStatusIsBounded(t *testing.T) {
	for status, want := range map[string]string{
		"succeeded": "observed_success",
		"failed":    "observed_failure",
		"attempted": "in_progress",
		"unknown":   "unknown",
		"":          "unknown",
	} {
		if got := exportConnectionStatus(status); got != want {
			t.Fatalf("exportConnectionStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestAnalyticsExportHealthResponseHidesProviderDetails(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	srv := &Server{analytics: &analytics.Manager{}}
	row := &generated.GetExportHealthStatsRow{
		SuccessCount:      2,
		FailureCount:      1,
		AttemptedCount:    1,
		ExportedBytes:     4096,
		BytesObservations: 2,
		AvgLatencyMs:      37.5,
		MaxLatencyMs:      50,
		LatestAttemptAt:   pgtype.Timestamptz{Time: now, Valid: true},
		LatestSuccessAt:   pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		LatestFailureAt:   pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		LatestStatus:      "succeeded",
		LatestErrorCode:   "cold_write",
	}
	got := toAnalyticsExportHealthResponse(srv, row)
	if got.ColdTier != "local" || got.ConnectionStatus != "observed_success" || !got.Configured {
		t.Fatalf("response state = %#v, want configured local observed_success", got)
	}
	if got.RetentionStatus != "observed_success" || got.QuotaStatus != "not_configured" {
		t.Fatalf("retention/quota state = %q/%q, want observed_success/not_configured", got.RetentionStatus, got.QuotaStatus)
	}
	if !got.BytesKnown || got.ExportedBytes != 4096 || got.BytesObservations != 2 {
		t.Fatalf("response bytes = %#v, want known 4096/2", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, forbidden := range []string{"minio.internal", "9000", "secret", "password", "payload", "bucket"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("analytics health leaked %q: %s", forbidden, body)
		}
	}
}

func TestAnalyticsExportHealthS3BytesRemainUnknown(t *testing.T) {
	srv := &Server{cfg: config.Config{S3Endpoint: "minio"}, analytics: &analytics.Manager{}}
	row := &generated.GetExportHealthStatsRow{
		SuccessCount:      1,
		ExportedBytes:     0,
		BytesObservations: 0,
		LatestStatus:      "succeeded",
	}
	got := toAnalyticsExportHealthResponse(srv, row)
	if got.BytesKnown {
		t.Fatal("S3 export health claimed byte size despite httpfs not exposing remote object length")
	}
	if got.ConnectionStatus != "observed_success" {
		t.Fatalf("connection status = %q, want observed_success", got.ConnectionStatus)
	}
	if got.RetentionStatus != "observed_success" || got.QuotaStatus != "provider_unknown" {
		t.Fatalf("retention/quota state = %q/%q, want observed_success/provider_unknown", got.RetentionStatus, got.QuotaStatus)
	}
}

func TestRetentionAndQuotaStatusesStayExplicit(t *testing.T) {
	cases := []struct {
		name      string
		srv       *Server
		latest    string
		retention string
		quota     string
	}{
		{name: "analytics paused", srv: &Server{}, latest: "unknown", retention: "paused", quota: "not_configured"},
		{name: "drop without export", srv: &Server{cfg: config.Config{AllowDropWithoutExport: true}}, latest: "failed", retention: "drop_without_export", quota: "not_configured"},
		{name: "remote provider quota unknown", srv: &Server{cfg: config.Config{S3Endpoint: "minio"}, analytics: &analytics.Manager{}}, latest: "failed", retention: "observed_failure", quota: "provider_unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retentionExportStatus(tc.srv, tc.latest); got != tc.retention {
				t.Fatalf("retentionExportStatus = %q, want %q", got, tc.retention)
			}
			if got := objectStoreQuotaStatus(tc.srv); got != tc.quota {
				t.Fatalf("objectStoreQuotaStatus = %q, want %q", got, tc.quota)
			}
		})
	}
}

func TestAnalyticsHealthRouteIsAuthenticatedRead(t *testing.T) {
	b, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	route := `r.Get("/analytics-health", s.handleAnalyticsHealth)`
	at := strings.Index(body, route)
	if at < 0 {
		t.Fatalf("missing %s", route)
	}
	api := strings.Index(body, `r.Route("/api"`)
	if api < 0 || at < api {
		t.Fatal("analytics health route must stay inside the authenticated API router")
	}
	writes := strings.Index(body[api:], "// Writes: member+ (viewers are read-only).")
	if writes < 0 || at >= api+writes {
		t.Fatal("analytics health route must be registered with viewer-readable routes")
	}
}
