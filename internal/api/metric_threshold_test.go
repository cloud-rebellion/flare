package api

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeMetricThreshold(t *testing.T) {
	zero := 0.0
	cases := []struct {
		name       string
		metric     string
		comparison string
		threshold  *float64
		window     int32
		wantMetric string
		wantCmp    string
		wantValue  float64
		wantErr    bool
	}{
		{name: "canonicalizes fields", metric: "  cpu.utilization ", comparison: " GT ", threshold: &zero, window: 15, wantMetric: "cpu.utilization", wantCmp: "gt", wantValue: 0},
		{name: "defaults comparison", metric: "queue.depth", threshold: &zero, window: 1, wantMetric: "queue.depth", wantCmp: "gte", wantValue: 0},
		{name: "missing metric", metric: " ", comparison: "gte", threshold: &zero, window: 1, wantErr: true},
		{name: "metric too long", metric: string(make([]byte, 129)), comparison: "gte", threshold: &zero, window: 1, wantErr: true},
		{name: "invalid comparison", metric: "queue.depth", comparison: "eq", threshold: &zero, window: 1, wantErr: true},
		{name: "window below bound", metric: "queue.depth", comparison: "gte", threshold: &zero, window: 0, wantErr: true},
		{name: "window above bound", metric: "queue.depth", comparison: "gte", threshold: &zero, window: 721, wantErr: true},
		{name: "threshold omitted", metric: "queue.depth", comparison: "gte", threshold: nil, window: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metric, comparison, value, err := normalizeMetricThreshold(tc.metric, tc.comparison, tc.threshold, tc.window)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if metric != tc.wantMetric || comparison != tc.wantCmp || value != tc.wantValue {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", metric, comparison, value, tc.wantMetric, tc.wantCmp, tc.wantValue)
			}
		})
	}

	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, _, err := normalizeMetricThreshold("queue.depth", "gte", &value, 1); err == nil {
			t.Errorf("non-finite threshold %v was accepted", value)
		}
	}
}

func TestNormalizeMetricLabels(t *testing.T) {
	got, err := normalizeMetricLabels(map[string]string{
		"environment":  "production",
		"release":      "web@abc123",
		"route":        "/checkout/:id",
		"method":       "GET",
		"status_class": "5xx",
	})
	if err != nil {
		t.Fatalf("normalize labels: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode labels: %v", err)
	}
	if decoded["environment"] != "production" || decoded["route"] != "/checkout/:id" || len(decoded) != 5 {
		t.Fatalf("labels = %#v", decoded)
	}
	if got, err := normalizeMetricLabels(nil); err != nil || string(got) != "{}" {
		t.Fatalf("empty labels = %s, err=%v", got, err)
	}
	for name, labels := range map[string]map[string]string{
		"unknown key": {"user_id": "42"},
		"bad status":  {"status_class": "503"},
		"bad method":  {"method": "get"},
		"bad token":   {"environment": "prod space"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeMetricLabels(labels); err == nil {
				t.Fatal("expected selector validation error")
			}
		})
	}
}

func TestMetricThresholdReason(t *testing.T) {
	stats := &generated.GetMetricThresholdStatsRow{Points: 3, MinValue: 2.5, MaxValue: 9.5}
	cases := []struct {
		name       string
		comparison string
		threshold  float64
		points     int64
		wantFire   bool
	}{
		{name: "greater than fires", comparison: "gt", threshold: 9, wantFire: true},
		{name: "greater than equal fires at max", comparison: "gte", threshold: 9.5, wantFire: true},
		{name: "greater than does not fire below max", comparison: "gt", threshold: 10, wantFire: false},
		{name: "less than fires from min", comparison: "lt", threshold: 3, wantFire: true},
		{name: "less than equal fires at min", comparison: "lte", threshold: 2.5, wantFire: true},
		{name: "less than does not fire below min", comparison: "lt", threshold: 2, wantFire: false},
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
			got := metricThresholdReason(&copy, "queue.depth", tc.comparison, tc.threshold, 15) != ""
			if got != tc.wantFire {
				t.Fatalf("fired=%v want %v", got, tc.wantFire)
			}
		})
	}

	if got := metricThresholdReason(nil, "queue.depth", "gte", 1, 15); got != "" {
		t.Fatalf("nil stats fired: %q", got)
	}
	if got := metricThresholdReason(stats, "queue.depth", "gte", math.NaN(), 15); got != "" {
		t.Fatalf("non-finite threshold fired: %q", got)
	}
}
