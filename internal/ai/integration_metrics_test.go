package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/metrics"
)

type aiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f aiRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func aiResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestCompleteRecordsBoundedIntegrationMetrics(t *testing.T) {
	previousMetrics := metrics.Default
	t.Cleanup(func() { metrics.Default = previousMetrics })
	m := metrics.New()
	metrics.Default = m
	calls := 0
	c := &Client{http: &http.Client{Transport: aiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return aiResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`), nil
		}
		return aiResponse(http.StatusBadGateway, "model-response-secret"), nil
	})}}

	if got, err := c.Complete(context.Background(), Config{
		BaseURL: "https://ai.example", APIKey: "key-secret", Model: "model-secret", Format: "openai",
	}, "system-secret", "user-secret"); err != nil || got != "ok" {
		t.Fatalf("successful completion = %q, %v", got, err)
	}
	if _, err := c.Complete(context.Background(), Config{
		BaseURL: "https://ai.example", APIKey: "key-secret", Model: "model-secret", Format: "anthropic",
	}, "system-secret", "user-secret"); err == nil {
		t.Fatal("failed completion should return an error")
	}

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		`flare_integration_requests_total{channel="openai",operation="complete",outcome="ok",provider="ai"} 1`,
		`flare_integration_requests_total{channel="anthropic",operation="complete",outcome="error",provider="ai"} 1`,
		`flare_integration_request_duration_seconds_count{channel="openai",operation="complete",outcome="ok",provider="ai"} 1`,
		`flare_integration_request_duration_seconds_count{channel="anthropic",operation="complete",outcome="error",provider="ai"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"ai.example", "key-secret", "model-secret", "system-secret", "user-secret", "model-response-secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("AI value leaked into metrics: %q\n%s", forbidden, body)
		}
	}
}
