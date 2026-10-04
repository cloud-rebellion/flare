package ai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/integration"
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

func TestCompleteReceiptFailurePreservesModelReply(t *testing.T) {
	previousLogger := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		integration.SetRecorder(nil)
	})
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	providerCalls, receiptCalls := 0, 0
	client := &Client{http: &http.Client{Transport: aiRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return aiResponse(http.StatusOK, `{"choices":[{"message":{"content":"model reply"}}]}`), nil
	})}}
	integration.SetRecorderWithError(func(_ context.Context, a integration.Attempt) error {
		receiptCalls++
		if a.Scope.OrgID != "org-1" || a.Provider != "ai" || a.Operation != "complete" || a.Outcome != "ok" {
			t.Fatalf("model receipt = %#v", a)
		}
		return errors.New("storage-secret")
	})
	ctx := integration.WithScope(context.Background(), integration.Scope{OrgID: "org-1"})
	reply, err := client.Complete(ctx, Config{BaseURL: "https://ai.example", APIKey: "key-secret", Model: "test"}, "system", "user")
	if err != nil || reply != "model reply" || providerCalls != 1 || receiptCalls != 1 {
		t.Fatalf("reply = %q, err = %v, provider calls = %d, receipt calls = %d", reply, err, providerCalls, receiptCalls)
	}
	if !strings.Contains(logs.String(), "receipt_status=unavailable") || strings.Contains(logs.String(), "storage-secret") || strings.Contains(logs.String(), "key-secret") {
		t.Fatalf("receipt failure log must use fixed status: %s", logs.String())
	}
}
