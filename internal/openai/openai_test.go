package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/connectorcheck"
	"github.com/bright-interaction/flare/internal/integration"
)

func TestCheckSuccessUsesFixedModelsAuthorityAndDiscardsBody(t *testing.T) {
	const token = "sk-secret"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != modelsPath || r.Header.Get("Authorization") != "Bearer "+token {
			return nil, errors.New("unexpected probe request")
		}
		header := http.Header{}
		header.Set("x-ratelimit-remaining-requests", "17")
		return fakeResponse(http.StatusOK, header, `{"object":"list","data":[{"id":"model-secret"}]}`), nil
	})
	var got integration.Attempt
	integration.SetRecorder(func(_ context.Context, attempt integration.Attempt) { got = attempt })
	defer integration.SetRecorder(nil)
	result, err := (Client{baseURL: defaultBaseURL, httpClient: &http.Client{Transport: transport}}).Check(
		integration.WithScope(context.Background(), integration.Scope{OrgID: "org-1"}), token,
	)
	if err != nil || result.Outcome != "ok" || result.StatusClass != "2xx" {
		t.Fatalf("success result = %#v, err %v", result, err)
	}
	if result.QuotaRemaining == nil || *result.QuotaRemaining != 17 {
		t.Fatalf("quota hint = %#v", result.QuotaRemaining)
	}
	if got.Provider != "openai" || got.Operation != probeOperation || got.Outcome != "ok" || got.Scope.OrgID != "org-1" {
		t.Fatalf("receipt = %#v", got)
	}
	if strings.Contains(got.ErrorCode, "model-secret") {
		t.Fatalf("receipt leaked model metadata: %#v", got)
	}
}

func TestCheckClassifiesAuthRateLimitAndProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		header     string
		wantCode   string
		wantRetry  int64
		wantStatus string
	}{
		{name: "auth", status: http.StatusUnauthorized, wantCode: "auth_expired", wantStatus: "4xx"},
		{name: "rate", status: http.StatusTooManyRequests, header: "11", wantCode: "rate_limited", wantRetry: 11000, wantStatus: "4xx"},
		{name: "server", status: http.StatusBadGateway, wantCode: "provider_error", wantStatus: "5xx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				header := http.Header{}
				if tc.header != "" {
					header.Set("Retry-After", tc.header)
				}
				return fakeResponse(tc.status, header, `{"error":{"message":"secret-never-retain"}}`), nil
			})
			result, err := (Client{baseURL: defaultBaseURL, httpClient: &http.Client{Transport: transport}}).Check(context.Background(), "token")
			if err == nil || result.Outcome != "error" || result.ErrorCode != tc.wantCode || result.StatusClass != tc.wantStatus {
				t.Fatalf("result = %#v, err %v", result, err)
			}
			if tc.wantRetry > 0 && (result.RetryAfterMS == nil || *result.RetryAfterMS != tc.wantRetry) {
				t.Fatalf("retry = %#v, want %d", result.RetryAfterMS, tc.wantRetry)
			}
			if strings.Contains(err.Error(), "secret-never-retain") {
				t.Fatalf("provider body leaked in error: %v", err)
			}
		})
	}
}

func TestCheckBoundsTokenAndHints(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return fakeResponse(http.StatusOK, http.Header{}, `{}`), nil
	})
	client := Client{baseURL: defaultBaseURL, httpClient: &http.Client{Transport: transport}}
	for _, token := range []string{"", "line\nfeed", strings.Repeat("x", maxProbeTokenLength+1)} {
		result, err := client.Check(context.Background(), token)
		if err == nil || result.ErrorCode != "invalid_config" {
			t.Fatalf("invalid token %q result = %#v, err %v", token, result, err)
		}
	}
	if got := quotaRemaining("-1"); got != nil {
		t.Fatalf("negative quota = %v", *got)
	}
	if got := quotaRemaining("1000000000000001"); got != nil {
		t.Fatalf("oversized quota = %v", *got)
	}
	if got := retryAfterMS("604801"); got != nil {
		t.Fatalf("oversized retry-after = %v", *got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeResponse(status int, header http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     statusClass(status),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

var _ connectorcheck.Probe = Client{}
