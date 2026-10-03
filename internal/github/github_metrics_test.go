package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/metrics"
)

type githubRoundTripFunc func(*http.Request) (*http.Response, error)

func (f githubRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func githubResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func githubMetricsOutput(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return out.Body.String()
}

func TestCreateIssueRecordsBoundedIntegrationMetrics(t *testing.T) {
	previousClient := httpClient
	previousMetrics := metrics.Default
	t.Cleanup(func() {
		httpClient = previousClient
		metrics.Default = previousMetrics
	})
	m := metrics.New()
	metrics.Default = m
	calls := 0
	httpClient = &http.Client{Transport: githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.github.com" || r.URL.Path != "/repos/acme/app/issues" {
			t.Fatalf("unexpected GitHub request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer token-secret" {
			t.Fatalf("missing GitHub authorization")
		}
		if calls == 1 {
			return githubResponse(http.StatusCreated, `{"html_url":"https://github.com/acme/app/issues/1"}`), nil
		}
		return githubResponse(http.StatusBadGateway, "response-secret"), nil
	})}

	if got, err := CreateIssue(context.Background(), "token-secret", "acme/app", "title-secret", "body-secret"); err != nil || got == "" {
		t.Fatalf("successful create = %q, %v", got, err)
	}
	if _, err := CreateIssue(context.Background(), "token-secret", "acme/app", "title-secret", "body-secret"); err == nil {
		t.Fatal("failed create should return an error")
	}

	out := githubMetricsOutput(t, m)
	for _, want := range []string{
		`flare_integration_requests_total{channel="http",operation="create_issue",outcome="ok",provider="github"} 1`,
		`flare_integration_requests_total{channel="http",operation="create_issue",outcome="error",provider="github"} 1`,
		`flare_integration_request_duration_seconds_count{channel="http",operation="create_issue",outcome="ok",provider="github"} 1`,
		`flare_integration_request_duration_seconds_count{channel="http",operation="create_issue",outcome="error",provider="github"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"acme/app", "token-secret", "title-secret", "response-secret"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("GitHub value leaked into metrics: %q\n%s", forbidden, out)
		}
	}
}
