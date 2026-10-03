package api

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeTracePercentile(t *testing.T) {
	threshold := 250.0
	cases := []struct {
		name           string
		span           string
		comparison     string
		percentile     int32
		window         int32
		threshold      *float64
		wantSpan       string
		wantComparison string
		wantPercentile int32
		wantThreshold  float64
		wantErr        bool
	}{
		{name: "canonicalizes fields", span: "  GET /checkout ", comparison: " GT ", percentile: 99, window: 15, threshold: &threshold, wantSpan: "GET /checkout", wantComparison: "gt", wantPercentile: 99, wantThreshold: 250},
		{name: "defaults percentile and comparison", span: "worker", window: 1, threshold: &threshold, wantSpan: "worker", wantComparison: "gte", wantPercentile: 95, wantThreshold: 250},
		{name: "missing span", span: " ", window: 1, threshold: &threshold, wantErr: true},
		{name: "span too long", span: string(make([]byte, 129)), window: 1, threshold: &threshold, wantErr: true},
		{name: "invalid percentile", span: "GET /", percentile: 80, window: 1, threshold: &threshold, wantErr: true},
		{name: "invalid comparison", span: "GET /", comparison: "eq", window: 1, threshold: &threshold, wantErr: true},
		{name: "window below bound", span: "GET /", window: 0, threshold: &threshold, wantErr: true},
		{name: "window above bound", span: "GET /", window: 721, threshold: &threshold, wantErr: true},
		{name: "threshold omitted", span: "GET /", window: 1, threshold: nil, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			span, comparison, percentile, gotThreshold, err := normalizeTracePercentile(tc.span, tc.comparison, tc.percentile, tc.threshold, tc.window)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if span != tc.wantSpan || comparison != tc.wantComparison || percentile != tc.wantPercentile || gotThreshold != tc.wantThreshold {
				t.Fatalf("got (%q, %q, %d, %v), want (%q, %q, %d, %v)", span, comparison, percentile, gotThreshold, tc.wantSpan, tc.wantComparison, tc.wantPercentile, tc.wantThreshold)
			}
		})
	}

	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 24*60*60*1000 + 1} {
		if _, _, _, _, err := normalizeTracePercentile("GET /", "gte", 95, &value, 1); err == nil {
			t.Errorf("invalid threshold %v was accepted", value)
		}
	}
}

func TestTracePercentileReason(t *testing.T) {
	stats := &generated.GetTracePercentileStatsRow{Points: 10, PercentileValue: 320}
	cases := []struct {
		name       string
		comparison string
		threshold  float64
		points     int64
		wantFire   bool
	}{
		{name: "greater than fires", comparison: "gt", threshold: 300, wantFire: true},
		{name: "greater equal fires at value", comparison: "gte", threshold: 320, wantFire: true},
		{name: "greater than stays quiet below threshold", comparison: "gt", threshold: 400, wantFire: false},
		{name: "less than fires", comparison: "lt", threshold: 400, wantFire: true},
		{name: "less than equal fires at value", comparison: "lte", threshold: 320, wantFire: true},
		{name: "less than stays quiet below value", comparison: "lt", threshold: 300, wantFire: false},
		{name: "unknown comparison is safe", comparison: "eq", threshold: 1, wantFire: false},
		{name: "empty series does not fire", comparison: "gte", threshold: 1, points: 0, wantFire: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copy := *stats
			copy.Points = tc.points
			if tc.points == 0 && tc.name != "empty series does not fire" {
				copy.Points = stats.Points
			}
			got := tracePercentileReason(&copy, "GET /checkout", 95, tc.comparison, tc.threshold, 15) != ""
			if got != tc.wantFire {
				t.Fatalf("fired=%v want %v", got, tc.wantFire)
			}
		})
	}

	if got := tracePercentileReason(nil, "GET /", 95, "gte", 1, 15); got != "" {
		t.Fatalf("nil stats fired: %q", got)
	}
	if got := tracePercentileReason(stats, "GET /", 95, "gte", math.NaN(), 15); got != "" {
		t.Fatalf("non-finite threshold fired: %q", got)
	}
}

func TestTracePercentileQueryScopesTenantAndOperation(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	sql := string(raw)
	start := strings.Index(sql, "-- name: GetTracePercentileStats")
	if start < 0 {
		t.Fatal("trace percentile query missing")
	}
	body := sql[start:]
	for _, required := range []string{"project_id = sqlc.arg(project_id)", "org_id = sqlc.arg(org_id)", "name = sqlc.arg(span_name)", "start_time >= sqlc.arg(since)", "percentile_cont"} {
		if !strings.Contains(body, required) {
			t.Fatalf("trace percentile query missing %q", required)
		}
	}
	for _, forbidden := range []string{"attributes", "trace_id", "span_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("trace percentile query selected raw span field %q", forbidden)
		}
	}
}
