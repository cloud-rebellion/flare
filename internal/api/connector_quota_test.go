package api

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeConnectorAttemptQuotaHints(t *testing.T) {
	remaining := int64(42)
	retry := int64(2500)
	got, err := normalizeConnectorAttemptRequest(connectorAttemptRequest{
		Provider: "hubspot", ConnectionID: "conn-prod", Operation: "contacts.list", Channel: "http", Outcome: "error",
		ErrorCode: "rate_limited", QuotaRemaining: &remaining, RetryAfterMS: &retry,
	})
	if err != nil {
		t.Fatalf("normalize valid hints: %v", err)
	}
	if got.QuotaRemaining == nil || *got.QuotaRemaining != remaining || got.RetryAfterMS == nil || *got.RetryAfterMS != retry {
		t.Fatalf("normalized quota hints = %#v", got)
	}
	if got.ConnectionID != "conn-prod" {
		t.Fatalf("normalized connection id = %q", got.ConnectionID)
	}
	lag := int64(12_500)
	got, err = normalizeConnectorAttemptRequest(connectorAttemptRequest{
		Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "ok", SyncLagMS: &lag,
	})
	if err != nil || got.SyncLagMS == nil || *got.SyncLagMS != lag {
		t.Fatalf("normalized sync lag = %#v, err %v", got.SyncLagMS, err)
	}
	tooHigh := maxConnectorQuotaRemaining + 1
	tooLong := maxConnectorRetryAfterMS + 1
	tooLag := maxConnectorSyncLagMS + 1
	for _, tc := range []struct {
		name string
		in   connectorAttemptRequest
	}{
		{"negative quota", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "ok", QuotaRemaining: ptrInt64(-1)}},
		{"oversized quota", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "ok", QuotaRemaining: &tooHigh}},
		{"negative retry", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "ok", RetryAfterMS: ptrInt64(-1)}},
		{"oversized retry", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "ok", RetryAfterMS: &tooLong}},
		{"oversized sync lag", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "ok", SyncLagMS: &tooLag}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeConnectorAttemptRequest(tc.in); err == nil {
				t.Fatal("expected bounded metadata validation error")
			}
		})
	}
	if _, err := normalizeConnectorAttemptRequest(connectorAttemptRequest{Provider: "hubspot", ConnectionID: "connection with spaces", Operation: "sync", Channel: "http", Outcome: "ok"}); err == nil {
		t.Fatal("unsafe connector connection id was accepted")
	}
}

func TestNormalizeConnectorQuota(t *testing.T) {
	threshold := 20.0
	provider, operation, channel, metric, comparison, value, err := normalizeConnectorQuota(
		" HubSpot ", " contacts.list ", " HTTP ", " quota_remaining ", " LTE ", &threshold, 15,
	)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if provider != "hubspot" || operation != "contacts.list" || channel != "http" || metric != "quota_remaining" || comparison != "lte" || value != threshold {
		t.Fatalf("normalized connector quota = %q/%q/%q/%q/%q/%v", provider, operation, channel, metric, comparison, value)
	}
	if _, _, _, metric, comparison, _, err = normalizeConnectorQuota("hubspot", "", "", "", "", &threshold, 1); err != nil || metric != "quota_remaining" || comparison != "lte" {
		t.Fatalf("defaults = metric %q comparison %q err %v", metric, comparison, err)
	}
	for _, tc := range []struct {
		name     string
		provider string
		metric   string
		cmp      string
		value    float64
		window   int32
	}{
		{"missing provider", "", "quota_remaining", "lte", threshold, 15},
		{"bad metric", "hubspot", "tokens", "lte", threshold, 15},
		{"bad comparison", "hubspot", "quota_remaining", "eq", threshold, 15},
		{"negative threshold", "hubspot", "quota_remaining", "lte", -1, 15},
		{"nan threshold", "hubspot", "quota_remaining", "lte", math.NaN(), 15},
		{"too wide retry threshold", "hubspot", "retry_after_ms", "lte", float64(maxConnectorRetryAfterMS) + 1, 15},
		{"too wide sync lag threshold", "hubspot", "sync_lag_ms", "lte", float64(maxConnectorSyncLagMS) + 1, 15},
		{"bad window", "hubspot", "quota_remaining", "lte", threshold, 721},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, _, _, err := normalizeConnectorQuota(tc.provider, "", "", tc.metric, tc.cmp, &tc.value, tc.window); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestConnectorQuotaReason(t *testing.T) {
	stats := &generated.GetConnectorQuotaStatsRow{Points: 3, LatestValue: 4}
	if got := connectorQuotaReason(stats, "hubspot", "contacts.list", "http", "quota_remaining", "lte", 5, 15); got == "" || !strings.Contains(got, "quota remaining") {
		t.Fatalf("expected quota alert, got %q", got)
	}
	if got := connectorQuotaReason(stats, "hubspot", "", "", "quota_remaining", "lt", 4, 15); got != "" {
		t.Fatalf("strict equality should not fire: %q", got)
	}
	if got := connectorQuotaReason(stats, "hubspot", "", "", "quota_remaining", "lte", 3, 15); got != "" {
		t.Fatalf("value above threshold should not fire: %q", got)
	}
	retry := &generated.GetConnectorQuotaStatsRow{Points: 1, LatestValue: 12_000}
	if got := connectorQuotaReason(retry, "hubspot", "sync", "http", "retry_after_ms", "gte", 10_000, 5); got == "" || !strings.Contains(got, "retry-after") {
		t.Fatalf("expected retry alert, got %q", got)
	}
	lag := &generated.GetConnectorQuotaStatsRow{Points: 1, LatestValue: 12_000}
	if got := connectorQuotaReason(lag, "hubspot", "sync", "http", "sync_lag_ms", "gte", 10_000, 5); got == "" || !strings.Contains(got, "sync lag") {
		t.Fatalf("expected sync-lag alert, got %q", got)
	}
	for _, bad := range []*generated.GetConnectorQuotaStatsRow{nil, {}, {Points: 2, LatestValue: -1}} {
		if got := connectorQuotaReason(bad, "hubspot", "", "", "quota_remaining", "lte", 1, 5); got != "" {
			t.Fatalf("invalid series fired: %q", got)
		}
	}
}

func TestConnectorQuotaQueryKeepsTenantScopeAndLatestOrdering(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/connector_attempts.sql")
	if err != nil {
		t.Fatalf("read connector queries: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetConnectorQuotaStats")
	if start < 0 {
		t.Fatal("quota watchdog query is missing")
	}
	section := body[start:]
	for _, want := range []string{
		"COALESCE(project_id, '') = sqlc.arg(project_id)::text",
		"org_id = sqlc.arg(org_id)::text",
		"provider = sqlc.arg(provider)::text",
		"sqlc.arg(connection_id)::text = '' OR connection_id = sqlc.arg(connection_id)::text",
		"ORDER BY completed_at DESC",
		"LIMIT 1",
		"sync_lag_ms",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("quota query missing %q", want)
		}
	}
}

func TestProjectConnectorQuotaQueryKeepsProjectAndTenantScope(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/connector_attempts.sql")
	if err != nil {
		t.Fatalf("read connector queries: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: ListConnectorQuotaHealthByProject")
	if start < 0 {
		t.Fatal("project quota health query is missing")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: ListConnectorQuotaHealthByProject"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: ListConnectorQuotaHealthByProject")+next]
	}
	for _, want := range []string{
		"ca.project_id = $1",
		"ca.org_id = $2",
		"ca.completed_at >= $3",
		"GROUP BY COALESCE(ca.connection_id, '')",
		"USING (connection_id, provider, operation, channel)",
		"ORDER BY s.last_completed_at DESC",
		"sync_lag_ms",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("project quota query missing %q", want)
		}
	}
}

func ptrInt64(v int64) *int64 { return &v }
