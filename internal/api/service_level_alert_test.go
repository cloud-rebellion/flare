package api

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeServiceLevel(t *testing.T) {
	tests := []struct {
		name        string
		span        string
		target      *float64
		latency     int32
		window      int32
		wantSpan    string
		wantTarget  float64
		wantLatency int32
		wantErr     bool
	}{
		{name: "canonical values", span: " GET /checkout ", target: floatPtr(0.999), latency: 750, window: 15, wantSpan: "GET /checkout", wantTarget: 0.999, wantLatency: 750},
		{name: "defaults target and latency", span: "GET /", latency: 0, window: 5, wantSpan: "GET /", wantTarget: 0.999, wantLatency: 1000},
		{name: "missing span", span: " ", target: floatPtr(0.999), latency: 500, window: 5, wantErr: true},
		{name: "bad target", span: "GET /", target: floatPtr(1), latency: 500, window: 5, wantErr: true},
		{name: "nan target", span: "GET /", target: floatPtr(math.NaN()), latency: 500, window: 5, wantErr: true},
		{name: "bad latency", span: "GET /", target: floatPtr(0.99), latency: -1, window: 5, wantErr: true},
		{name: "bad window", span: "GET /", target: floatPtr(0.99), latency: 500, window: 721, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			span, target, latency, window, err := normalizeServiceLevel(tc.span, tc.target, tc.latency, tc.window)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeServiceLevel: %v", err)
			}
			if span != tc.wantSpan || target != tc.wantTarget || latency != tc.wantLatency || window != tc.window {
				t.Fatalf("got span=%q target=%v latency=%d window=%d", span, target, latency, window)
			}
		})
	}
}

func TestServiceLevelReason(t *testing.T) {
	good := &generated.GetServiceLevelStatsRow{SpanCount: 1000, GoodCount: 999}
	if got := serviceLevelReason(good, "GET /checkout", 750, 0.999, 15); got != "" {
		t.Fatalf("target met should stay quiet: %q", got)
	}
	bad := &generated.GetServiceLevelStatsRow{SpanCount: 1000, GoodCount: 998}
	got := serviceLevelReason(bad, "GET /checkout", 750, 0.999, 15)
	if !strings.Contains(got, "availability 0.99800 below target 0.99900") || !strings.Contains(got, "good 998/1000") {
		t.Fatalf("reason did not expose bounded aggregate: %q", got)
	}
	if got := serviceLevelReason(&generated.GetServiceLevelStatsRow{SpanCount: 0}, "GET /checkout", 750, 0.999, 15); got != "" {
		t.Fatalf("missing span series should stay quiet: %q", got)
	}
}

func TestServiceLevelWatchdogQueryKeepsTenantScope(t *testing.T) {
	queryBytes, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog query: %v", err)
	}
	query := string(queryBytes)
	start := strings.Index(query, "-- name: GetServiceLevelStats :one")
	if start < 0 {
		t.Fatal("service-level query is missing")
	}
	query = query[start:]
	for _, want := range []string{"project_id = $2", "org_id = $3", "name = $4", "start_time >= $5", "duration_ms <= $1"} {
		// The source uses sqlc.arg names; generated SQL is covered by sqlc's
		// compile check, while this assertion keeps the tenant predicates in the
		// source query itself explicit.
		want = strings.ReplaceAll(want, "$2", "sqlc.arg(project_id)")
		want = strings.ReplaceAll(want, "$3", "sqlc.arg(org_id)")
		want = strings.ReplaceAll(want, "$4", "sqlc.arg(span_name)")
		want = strings.ReplaceAll(want, "$5", "sqlc.arg(since)")
		want = strings.ReplaceAll(want, "$1", "sqlc.arg(latency_ms)")
		if !strings.Contains(query, want) {
			t.Fatalf("service-level query missing %q: %s", want, query)
		}
	}
}

func floatPtr(value float64) *float64 { return &value }
