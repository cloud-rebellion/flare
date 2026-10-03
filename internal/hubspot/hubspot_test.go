package hubspot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/integration"
)

func TestCheckSuccessRecordsOnlyBoundedHints(t *testing.T) {
	const token = "hs_private_secret"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != accountDetailsPath || r.Header.Get("Authorization") != "Bearer "+token {
			return nil, errors.New("unexpected probe request")
		}
		header := http.Header{}
		header.Set("X-HubSpot-RateLimit-Daily-Remaining", "100")
		header.Set("X-HubSpot-RateLimit-Secondly-Remaining", "7")
		header.Set("Retry-After", "3")
		return fakeResponse(http.StatusOK, header, `{"portalId":123,"email":"person@example.com","secret":"never-retain"}`), nil
	})

	var got integration.Attempt
	integration.SetRecorder(func(_ context.Context, attempt integration.Attempt) { got = attempt })
	defer integration.SetRecorder(nil)
	client := Client{baseURL: "https://api.hubapi.com", httpClient: &http.Client{Transport: transport}}
	result, err := client.Check(integration.WithScope(context.Background(), integration.Scope{OrgID: "org-1"}), token)
	if err != nil || result.Outcome != "ok" || result.StatusClass != "2xx" {
		t.Fatalf("success result = %#v, err %v", result, err)
	}
	if result.QuotaRemaining == nil || *result.QuotaRemaining != 7 || result.RetryAfterMS == nil || *result.RetryAfterMS != 3000 {
		t.Fatalf("bounded hints = %#v", result)
	}
	if got.Provider != "hubspot" || got.Operation != "account_details" || got.Outcome != "ok" || got.Scope.OrgID != "org-1" {
		t.Fatalf("receipt = %#v", got)
	}
	encoded := strings.Join([]string{got.Provider, got.Operation, got.ErrorCode}, " ")
	if strings.Contains(encoded, token) || strings.Contains(encoded, "person@example.com") {
		t.Fatalf("receipt leaked provider data: %q", encoded)
	}
}

func TestCheckClassifiesAuthAndRateLimitWithoutBody(t *testing.T) {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			const secret = "token-never-in-error"
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer "+secret {
					return nil, errors.New("authorization header did not reach provider")
				}
				header := http.Header{}
				if tc.header != "" {
					header.Set("Retry-After", tc.header)
				}
				return fakeResponse(tc.status, header, `{"message":"`+secret+`"}`), nil
			})
			result, err := (Client{baseURL: "https://api.hubapi.com", httpClient: &http.Client{Transport: transport}}).Check(context.Background(), secret)
			if err == nil || result.ErrorCode != tc.wantCode || result.StatusClass != tc.wantStatus {
				t.Fatalf("result = %#v, err %v", result, err)
			}
			if tc.wantRetry > 0 && (result.RetryAfterMS == nil || *result.RetryAfterMS != tc.wantRetry) {
				t.Fatalf("retry = %#v, want %d", result.RetryAfterMS, tc.wantRetry)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked token: %v", err)
			}
		})
	}
}

func TestCheckHandlesTimeoutAndInvalidToken(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	client := Client{baseURL: "https://api.hubapi.com", httpClient: &http.Client{Transport: transport}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := client.Check(ctx, "token")
	if err == nil || result.ErrorCode != "timeout" || result.Outcome != "error" {
		t.Fatalf("timeout result = %#v, err %v", result, err)
	}
	for _, token := range []string{"", "line\nfeed", strings.Repeat("x", maxProbeTokenLength+1)} {
		result, err := New().Check(context.Background(), token)
		if err == nil || result.ErrorCode != "invalid_config" {
			t.Fatalf("invalid token %q result = %#v, err %v", token, result, err)
		}
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

func TestRetryAfterAndStatusBounds(t *testing.T) {
	if got := retryAfterMS("604801"); got != nil {
		t.Fatalf("oversized retry-after = %v", *got)
	}
	if got := retryAfterMS("not-a-duration"); got != nil {
		t.Fatalf("invalid retry-after = %v", *got)
	}
	if got := statusClass(700); got != "" {
		t.Fatalf("invalid status class = %q", got)
	}
}

func TestNewDoesNotFollowRedirects(t *testing.T) {
	client := New().httpClient
	if client == nil || client.CheckRedirect == nil {
		t.Fatal("HubSpot client must install a redirect guard")
	}
	if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect error = %v, want http.ErrUseLastResponse", err)
	}
}
