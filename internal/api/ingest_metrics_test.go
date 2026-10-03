package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/ingest"
	"github.com/bright-interaction/flare/internal/metrics"
)

func TestClassifyIngestRequest(t *testing.T) {
	tests := []struct {
		name string
		in   ingestOutcome
		want string
	}{
		{"accepted", ingestOutcome{accepted: 3}, "accepted"},
		{"partial", ingestOutcome{accepted: 2, dropped: 1}, "partial"},
		{"failed", ingestOutcome{failed: 2}, "failed"},
		{"rejected", ingestOutcome{invalid: 1}, "rejected"},
		{"empty", ingestOutcome{}, "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyIngestRequest(tt.in); got != tt.want {
				t.Fatalf("classifyIngestRequest(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMetricNonFiniteValuesAreRejectedAndCounted(t *testing.T) {
	records := []ingest.MetricRecord{
		{Name: "ok", Value: 1},
		{Name: "nan", Value: math.NaN()},
		{Name: "positive-inf", Value: math.Inf(1)},
		{Name: "negative-inf", Value: math.Inf(-1)},
	}
	budget := newIngestBudget()
	got := finiteMetricRows(records[:budget.take(len(records))], budget)
	if len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("persistable metrics = %+v, want only finite point", got)
	}
	if budget.invalid != 3 || budget.rejected() != 3 {
		t.Fatalf("budget = %+v, want three invalid metrics", budget)
	}
}

func TestMetricOversizedLabelsAreRejectedWithoutTruncation(t *testing.T) {
	const prefix, suffix = `{"v":"`, `"}`
	underLimit := json.RawMessage(prefix + strings.Repeat("x", maxMetricLabelBytes-len(prefix)-len(suffix)) + suffix)
	overLimit := json.RawMessage(prefix + strings.Repeat("x", maxMetricLabelBytes-len(prefix)-len(suffix)+1) + suffix)
	records := []ingest.MetricRecord{
		{Name: "bounded", Value: 1, Labels: underLimit},
		{Name: "oversized", Value: 2, Labels: overLimit},
	}
	budget := newIngestBudget()
	got := finiteMetricRows(records[:budget.take(len(records))], budget)
	if len(got) != 1 || got[0].Name != "bounded" {
		t.Fatalf("persistable metrics = %+v, want only the bounded-label point", got)
	}
	if budget.invalid != 1 || budget.rejected() != 1 {
		t.Fatalf("budget = %+v, want one invalid oversized-label metric", budget)
	}
	if len(got[0].Labels) != maxMetricLabelBytes {
		t.Fatalf("bounded labels were modified: got %d bytes, want %d", len(got[0].Labels), maxMetricLabelBytes)
	}
}

func TestRecordIngestRequestAndItemsExposeOnlyFixedLabels(t *testing.T) {
	m := metrics.New()
	previous := metrics.Default
	metrics.Default = m
	t.Cleanup(func() { metrics.Default = previous })

	recordIngestRequest("otlp", ingestOutcome{accepted: 3, dropped: 1}, "")
	ingestOutcome{accepted: 3, dropped: 1}.record("span")
	recordIngestRequest("untrusted-transport", ingestOutcome{}, "rejected")
	ingestOutcome{accepted: 99}.record("tenant-project-id")

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		`flare_ingest_requests_total{outcome="partial",transport="otlp"} 1`,
		`flare_ingest_requests_total{outcome="rejected",transport="other"} 1`,
		`flare_ingest_items_total{outcome="accepted",signal="span"} 3`,
		`flare_ingest_items_total{outcome="dropped",signal="span"} 1`,
		`flare_ingest_items_total{outcome="accepted",signal="other"} 99`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "tenant-project-id") || strings.Contains(body, "untrusted-transport") {
		t.Fatalf("untrusted labels escaped canonicalization:\n%s", body)
	}
}
