package api

import (
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeUsageThreshold(t *testing.T) {
	gotSurface, gotOperation, gotMeasure, gotComparison, gotThreshold, gotWindow, err := normalizeUsageThreshold(
		" MCP ", " tool_query_metrics ", " errors ", " GT ", 10, 15,
	)
	if err != nil {
		t.Fatalf("normalize usage threshold: %v", err)
	}
	if gotSurface != "mcp" || gotOperation != "tool_query_metrics" || gotMeasure != "errors" || gotComparison != "gt" || gotThreshold != 10 || gotWindow != 15 {
		t.Fatalf("normalized usage rule = %q/%q/%q/%q/%d/%d", gotSurface, gotOperation, gotMeasure, gotComparison, gotThreshold, gotWindow)
	}
	if surface, operation, _, _, _, _, err := normalizeUsageThreshold("business_key", "business_checks", "requests", "gte", 1, 5); err != nil || surface != "business_key" || operation != "business_checks" {
		t.Fatalf("business key usage rule = %q/%q (%v)", surface, operation, err)
	}
	for _, tc := range []struct {
		name       string
		surface    string
		operation  string
		measure    string
		comparison string
		threshold  int32
		window     int32
	}{
		{"unknown surface", "vendor", "read", "requests", "gte", 1, 5},
		{"unknown operation", "mcp", "inject", "requests", "gte", 1, 5},
		{"unknown measure", "mcp", "ping", "payload", "gte", 1, 5},
		{"unknown comparison", "mcp", "ping", "requests", "eq", 1, 5},
		{"zero threshold", "mcp", "ping", "requests", "gte", 0, 5},
		{"long window", "mcp", "ping", "requests", "gte", 1, 721},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, _, _, err := normalizeUsageThreshold(tc.surface, tc.operation, tc.measure, tc.comparison, tc.threshold, tc.window); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestUsageThresholdReasonDoesNotFireOnMissingSeries(t *testing.T) {
	missing := &generated.GetUsageThresholdStatsRow{BucketCount: 0, RequestCount: 0}
	for _, comparison := range []string{"lt", "lte", "gt", "gte"} {
		if got := usageThresholdReason(missing, "mcp", "ping", "requests", comparison, 1, 5); got != "" {
			t.Fatalf("missing series fired for %s: %q", comparison, got)
		}
	}
	stats := &generated.GetUsageThresholdStatsRow{BucketCount: 1, RequestCount: 10, ErrorCount: 2, DeniedCount: 1}
	if got := usageThresholdReason(stats, "mcp", "ping", "errors", "gte", 2, 5); got == "" || !strings.Contains(got, "errors") {
		t.Fatalf("error threshold did not fire: %q", got)
	}
	if got := usageThresholdReason(stats, "mcp", "ping", "requests", "gt", 10, 5); got != "" {
		t.Fatalf("strict request threshold fired at equality: %q", got)
	}
	if got := usageThresholdReason(stats, "mcp", "ping", "denied", "lte", 1, 5); got == "" {
		t.Fatal("denied lte threshold did not fire")
	}
}

func TestUsageThresholdQueryScopesTenantProjectAndOrgWideSurfaces(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	start := strings.Index(sql, "-- name: GetUsageThresholdStats")
	if start < 0 {
		t.Fatal("usage threshold query missing")
	}
	body := sql[start:]
	for _, required := range []string{
		"org_id = sqlc.arg(org_id)",
		"surface = sqlc.arg(surface)",
		"operation = sqlc.arg(operation)",
		"bucket_start >= sqlc.arg(since)",
		"project_id = sqlc.arg(project_id)",
		"surface IN ('api_key', 'mcp') AND project_id = ''",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("usage threshold query missing %q", required)
		}
	}
	for _, forbidden := range []string{"key_hash", "request_body", "response_body", "user_id", "session_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("usage threshold query contains forbidden field %q", forbidden)
		}
	}
}

func TestUsageThresholdMigrationAddsConstrainedDimensions(t *testing.T) {
	raw, err := os.ReadFile("../db/migrations/043_usage_threshold_alerts.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.ToLower(string(raw))
	for _, required := range []string{
		"add column usage_surface text not null default ''",
		"add column usage_operation text not null default ''",
		"add column usage_measure text not null default 'requests'",
		"check (usage_surface in ('', 'dsn', 'api_key', 'mcp'))",
		"check (usage_measure in ('requests', 'errors', 'denied'))",
	} {
		if !strings.Contains(migration, required) {
			t.Fatalf("usage alert migration missing %q", required)
		}
	}
}
