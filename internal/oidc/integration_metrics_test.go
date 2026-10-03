package oidc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/metrics"
)

type oidcRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oidcRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func oidcResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestOIDCOutboundCallsRecordBoundedIntegrationMetrics(t *testing.T) {
	previousClient := httpClient
	previousMetrics := metrics.Default
	t.Cleanup(func() {
		httpClient = previousClient
		metrics.Default = previousMetrics
	})
	m := metrics.New()
	metrics.Default = m
	issuer := "https://metrics-oidc-issuer.example"
	discMu.Lock()
	delete(discCache, issuer)
	discMu.Unlock()
	httpClient = &http.Client{Transport: oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			return oidcResponse(http.StatusOK, `{"authorization_endpoint":"https://metrics-oidc-issuer.example/authorize","token_endpoint":"https://metrics-oidc-issuer.example/token","userinfo_endpoint":"https://metrics-oidc-issuer.example/userinfo"}`), nil
		case "/token":
			return oidcResponse(http.StatusOK, `{"access_token":"access-secret"}`), nil
		case "/userinfo":
			return oidcResponse(http.StatusOK, `{"sub":"subject-secret","email":"person@example.com","email_verified":true}`), nil
		default:
			return oidcResponse(http.StatusNotFound, "unexpected path"), nil
		}
	})}

	provider, err := Discover(context.Background(), issuer)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	accessToken, err := provider.Exchange(context.Background(), "client-secret", "client-secret", "code-secret", "https://flare.example/callback")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := provider.FetchUserInfo(context.Background(), accessToken); err != nil {
		t.Fatalf("userinfo: %v", err)
	}

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		`flare_integration_requests_total{channel="http",operation="discover",outcome="ok",provider="oidc"} 1`,
		`flare_integration_requests_total{channel="http",operation="exchange",outcome="ok",provider="oidc"} 1`,
		`flare_integration_requests_total{channel="http",operation="fetch_userinfo",outcome="ok",provider="oidc"} 1`,
		`flare_integration_request_duration_seconds_count{channel="http",operation="discover",outcome="ok",provider="oidc"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"metrics-oidc-issuer.example", "client-secret", "access-secret", "code-secret", "subject-secret", "person@example.com"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("OIDC value leaked into metrics: %q\n%s", forbidden, body)
		}
	}
}
