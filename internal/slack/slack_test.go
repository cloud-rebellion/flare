package slack

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

func TestCheckSuccessDiscardsWorkspaceBody(t *testing.T) {
	const token = "xoxb-secret"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != authTestPath || r.Header.Get("Authorization") != "Bearer "+token {
			return nil, errors.New("unexpected probe request")
		}
		return fakeResponse(http.StatusOK, `{"ok":true,"team":"workspace-secret","url":"https://secret.example"}`), nil
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
	if got.Provider != "slack" || got.Operation != probeOperation || got.Outcome != "ok" || got.Scope.OrgID != "org-1" {
		t.Fatalf("receipt = %#v", got)
	}
	if strings.Contains(got.ErrorCode, "workspace-secret") || strings.Contains(got.ErrorCode, "secret.example") {
		t.Fatalf("receipt leaked workspace data: %#v", got)
	}
}

func TestCheckClassifiesSlackEnvelopeAndHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantCode string
		status               int
	}{
		{name: "invalid auth envelope", status: http.StatusOK, body: `{"ok":false,"error":"invalid_auth"}`, wantCode: "auth_expired"},
		{name: "missing scope envelope", status: http.StatusOK, body: `{"ok":false,"error":"missing_scope"}`, wantCode: "permission_denied"},
		{name: "rate limited envelope", status: http.StatusOK, body: `{"ok":false,"error":"ratelimited"}`, wantCode: "rate_limited"},
		{name: "forbidden", status: http.StatusForbidden, body: `{"ok":false,"error":"secret-never-retain"}`, wantCode: "auth_expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return fakeResponse(tc.status, tc.body), nil
			})
			result, err := (Client{baseURL: defaultBaseURL, httpClient: &http.Client{Transport: transport}}).Check(context.Background(), "token")
			if err == nil || result.Outcome != "error" || result.ErrorCode != tc.wantCode {
				t.Fatalf("result = %#v, err %v", result, err)
			}
			if strings.Contains(err.Error(), "secret-never-retain") {
				t.Fatalf("provider body leaked in error: %v", err)
			}
		})
	}
}

func TestCheckBoundsRetryAndToken(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		header := http.Header{}
		header.Set("Retry-After", "3")
		return fakeResponseWithHeader(http.StatusTooManyRequests, header, `{}`), nil
	})
	result, err := (Client{baseURL: defaultBaseURL, httpClient: &http.Client{Transport: transport}}).Check(context.Background(), "token")
	if err == nil || result.ErrorCode != "rate_limited" || result.RetryAfterMS == nil || *result.RetryAfterMS != 3000 {
		t.Fatalf("rate result = %#v, err %v", result, err)
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

func fakeResponse(status int, body string) *http.Response {
	return fakeResponseWithHeader(status, http.Header{}, body)
}

func fakeResponseWithHeader(status int, header http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     statusClass(status),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

var _ connectorcheck.Probe = Client{}
