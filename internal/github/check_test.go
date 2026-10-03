package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/connectorcheck"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/metrics"
)

func healthRoundTrip(status int, headers http.Header, body string) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func TestHealthCheckClassifiesFixedResponsesWithoutProviderPayload(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		headers    http.Header
		body       string
		wantResult connectorcheck.Result
		wantErr    bool
	}{
		{name: "success", status: http.StatusOK, body: `{"login":"private-user"}`, wantResult: connectorcheck.Result{Outcome: "ok", StatusClass: "2xx"}},
		{name: "auth", status: http.StatusUnauthorized, body: `{"message":"token-secret"}`, wantResult: connectorcheck.Result{Outcome: "error", StatusClass: "4xx", ErrorCode: "auth_expired"}, wantErr: true},
		{name: "permission", status: http.StatusForbidden, headers: http.Header{"X-RateLimit-Remaining": []string{"4"}}, body: `{"message":"private-org"}`, wantResult: connectorcheck.Result{Outcome: "error", StatusClass: "4xx", ErrorCode: "permission_denied"}, wantErr: true},
		{name: "rate limit", status: http.StatusForbidden, headers: http.Header{"X-Ratelimit-Remaining": []string{"0"}, "Retry-After": []string{"2"}}, body: `{"message":"quota-secret"}`, wantResult: connectorcheck.Result{Outcome: "error", StatusClass: "4xx", ErrorCode: "rate_limited"}, wantErr: true},
		{name: "provider", status: http.StatusBadGateway, body: `provider-secret`, wantResult: connectorcheck.Result{Outcome: "error", StatusClass: "5xx", ErrorCode: "provider_error"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := HealthClient{
				baseURL: "https://api.github.com",
				httpClient: &http.Client{Transport: githubRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Host != "api.github.com" || req.URL.Path != "/user" {
						t.Fatalf("unexpected GitHub request %s", req.URL)
					}
					if req.Header.Get("Authorization") != "Bearer token-secret" {
						t.Fatalf("missing GitHub authorization")
					}
					if req.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
						t.Fatalf("missing GitHub API version")
					}
					return healthRoundTrip(tc.status, tc.headers, tc.body), nil
				})},
			}
			got, err := client.Check(integration.WithScope(context.Background(), integration.Scope{OrgID: "org-1", ConnectionID: "conn-1"}), "token-secret")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if got.Outcome != tc.wantResult.Outcome || got.StatusClass != tc.wantResult.StatusClass || got.ErrorCode != tc.wantResult.ErrorCode {
				t.Fatalf("result = %#v, want %#v", got, tc.wantResult)
			}
			if got.QuotaRemaining != nil && *got.QuotaRemaining < 0 {
				t.Fatalf("negative quota in result: %#v", got)
			}
		})
	}
}

func TestHealthCheckRejectsUnsafeTokenAndDoesNotFollowRedirect(t *testing.T) {
	client := NewHealthClient()
	client.httpClient = &http.Client{Transport: githubRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return healthRoundTrip(http.StatusFound, http.Header{"Location": []string{"https://attacker.example/user"}}, "secret"), nil
	}), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if _, err := client.Check(context.Background(), "bad\nsecret"); err == nil {
		t.Fatal("control-character token was accepted")
	}
	got, err := client.Check(context.Background(), "token-secret")
	if err == nil || got.ErrorCode != "provider_error" || got.StatusClass != "3xx" {
		t.Fatalf("redirect result = %#v/%v, want bounded provider error", got, err)
	}
}

func TestHealthCheckTransportAndMetricsAreBounded(t *testing.T) {
	previousMetrics := metrics.Default
	t.Cleanup(func() { metrics.Default = previousMetrics })
	m := metrics.New()
	metrics.Default = m
	client := HealthClient{httpClient: &http.Client{Transport: githubRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport-secret")
	})}}
	got, err := client.Check(context.Background(), "token-secret")
	if err == nil || got.ErrorCode != "provider_error" || got.Outcome != "error" {
		t.Fatalf("transport result = %#v/%v", got, err)
	}
	body := githubMetricsOutput(t, m)
	if strings.Contains(body, "transport-secret") || strings.Contains(body, "token-secret") {
		t.Fatalf("secret leaked in metrics: %s", body)
	}
	if !strings.Contains(body, `provider="github"`) || !strings.Contains(body, `operation="user"`) {
		t.Fatalf("GitHub health metric missing: %s", body)
	}
}
