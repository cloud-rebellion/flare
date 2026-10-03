package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMiddlewareEmitsNormalizedLabelsAndBuckets(t *testing.T) {
	m := New()
	h := m.Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/42" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/users/42?token=secret", nil))
	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	if !strings.Contains(body, `method="GET",route="<unmatched>",status="404"`) {
		t.Fatalf("normalized request labels missing:\n%s", body)
	}
	if !strings.Contains(body, `le="+Inf"} 1`) {
		t.Fatalf("infinite bucket missing:\n%s", body)
	}
	if strings.Contains(body, "secret") {
		t.Fatal("query secret leaked into metrics")
	}
}

func TestPatternIsKeptAndUnknownMethodsDoNotCreatePathCardinality(t *testing.T) {
	m := New()
	h := m.Middleware(func(*http.Request) string { return "/api/projects/{id}" })(http.NotFoundHandler())
	for _, path := range []string{"/api/projects/1", "/api/projects/2"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
	}
	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	if !strings.Contains(body, `method="POST",route="/api/projects/{id}",status="404"} 2`) {
		t.Fatalf("expected one bounded series for both IDs:\n%s", body)
	}
	if strings.Contains(body, "/api/projects/1") || strings.Contains(body, "/api/projects/2") {
		t.Fatalf("raw IDs became labels:\n%s", body)
	}
}

func TestCustomCounterAndHistogramDoNotDoubleCount(t *testing.T) {
	m := New()
	labels := map[string]string{"integration": "smtp", "outcome": "ok"}
	m.Inc("flare_integration_requests", labels)
	m.Observe("flare_integration_request", labels, 0.02)
	m.Observe("flare_integration_request", labels, 0.03)

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	if !strings.Contains(body, `flare_integration_requests_total{integration="smtp",outcome="ok"} 1`) {
		t.Fatalf("counter should count Inc calls once:\n%s", body)
	}
	if !strings.Contains(body, `flare_integration_request_duration_seconds_count{integration="smtp",outcome="ok"} 2`) {
		t.Fatalf("histogram count should count Observe calls:\n%s", body)
	}
}

func TestCustomCounterAddAccumulatesDelta(t *testing.T) {
	m := New()
	m.Add("flare_ingest_items", map[string]string{"signal": "event", "outcome": "accepted"}, 3)
	m.Add("flare_ingest_items", map[string]string{"signal": "event", "outcome": "accepted"}, 2)
	m.Add("flare_ingest_items", map[string]string{"signal": "event", "outcome": "accepted"}, 0)

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(out.Body.String(), `flare_ingest_items_total{outcome="accepted",signal="event"} 5`) {
		t.Fatalf("counter delta not accumulated:\n%s", out.Body.String())
	}
}

func TestRecordIntegrationCanonicalizesUntrustedValues(t *testing.T) {
	m := New()
	previous := Default
	Default = m
	t.Cleanup(func() { Default = previous })

	RecordIntegration("github", "create_issue", "http", "ok", 10*time.Millisecond)
	RecordIntegration("github", "repo-secret", "repo-secret", "unexpected", time.Millisecond)

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	if !strings.Contains(body, `flare_integration_requests_total{channel="http",operation="create_issue",outcome="ok",provider="github"} 1`) {
		t.Fatalf("canonical integration series missing:\n%s", body)
	}
	if !strings.Contains(body, `flare_integration_requests_total{channel="other",operation="other",outcome="error",provider="github"} 1`) {
		t.Fatalf("unknown integration values were not collapsed:\n%s", body)
	}
	if strings.Contains(body, "repo-secret") || strings.Contains(body, "unexpected") {
		t.Fatalf("untrusted integration labels leaked:\n%s", body)
	}
}

func TestConnectorProbesKeepProviderIdentityInProcessMetrics(t *testing.T) {
	m := New()
	previous := Default
	Default = m
	t.Cleanup(func() { Default = previous })

	for _, probe := range []struct {
		provider  string
		operation string
		outcome   string
	}{
		{provider: "hubspot", operation: "account_details", outcome: "ok"},
		{provider: "slack", operation: "auth.test", outcome: "error"},
		{provider: "openai", operation: "models", outcome: "ok"},
	} {
		RecordIntegration(probe.provider, probe.operation, "http", probe.outcome, 10*time.Millisecond)
	}
	// A new adapter cannot use caller-controlled operation/channel values as
	// unbounded Prometheus labels even when its provider is allowlisted.
	RecordIntegration("hubspot", "account-secret-123", "https://secret.example", "unexpected", time.Millisecond)

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		`flare_integration_requests_total{channel="http",operation="account_details",outcome="ok",provider="hubspot"} 1`,
		`flare_integration_requests_total{channel="http",operation="auth.test",outcome="error",provider="slack"} 1`,
		`flare_integration_requests_total{channel="http",operation="models",outcome="ok",provider="openai"} 1`,
		`flare_integration_request_duration_seconds_count{channel="http",operation="account_details",outcome="ok",provider="hubspot"} 1`,
		`flare_integration_requests_total{channel="other",operation="other",outcome="error",provider="hubspot"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("integration metric missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "account-secret-123") || strings.Contains(body, "secret.example") {
		t.Fatalf("untrusted probe labels leaked:\n%s", body)
	}
}

func TestConnectorReceiptMetricsExposeStartAndCompletionOutcome(t *testing.T) {
	m := New()
	previous := Default
	Default = m
	t.Cleanup(func() { Default = previous })

	RecordConnectorReceipt("start", "started")
	RecordConnectorReceipt("complete", "ok")
	RecordConnectorReceipt("complete", "error")
	// Caller-controlled values collapse to a bounded error series rather than
	// becoming labels that can grow with provider or database text.
	RecordConnectorReceipt("insert-secret", "bearer-secret")

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		`flare_connector_receipts_total{operation="start",outcome="started"} 1`,
		`flare_connector_receipts_total{operation="complete",outcome="ok"} 1`,
		`flare_connector_receipts_total{operation="complete",outcome="error"} 1`,
		`flare_connector_receipts_total{operation="other",outcome="error"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("receipt metric missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("receipt metric leaked untrusted values:\n%s", body)
	}
}
