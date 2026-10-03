// Package hubspot contains the bounded health probe for a HubSpot
// installation. It deliberately reads account metadata only; the response
// body is discarded so CRM records and account details never enter Flare.
package hubspot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/flare/internal/connectorcheck"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/metrics"
)

const (
	defaultBaseURL      = "https://api.hubapi.com"
	accountDetailsPath  = "/account-info/v3/details"
	probeTimeout        = 12 * time.Second
	maxResponseBytes    = 64 << 10
	maxQuotaRemaining   = int64(1_000_000_000_000_000)
	maxRetryAfterMS     = int64(7 * 24 * 60 * 60 * 1000)
	maxProbeTokenLength = 16 << 10
)

// Result is kept as an alias for the provider-neutral worker and existing
// callers. It contains no account response, token, URL, or provider error
// text.
type Result = connectorcheck.Result

// Client probes one fixed HubSpot authority. baseURL and httpClient are kept
// on the value so package tests can use an httptest server without making the
// production path configurable or introducing an SSRF surface.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New returns the production HubSpot client. The provider authority is fixed
// to api.hubapi.com; connection credentials are supplied only per call.
func New() Client {
	return Client{baseURL: defaultBaseURL, httpClient: newHTTPClient()}
}

// Check performs a read-only account-details request using an OAuth/private
// app token. It drains and discards a bounded response body for connection
// reuse. Errors contain only a stable code and status class, never provider
// response text or the bearer token.
func (c Client) Check(ctx context.Context, token string) (result Result, err error) {
	startedAt := time.Now().UTC()
	started := time.Now()
	defer func() {
		outcome := result.Outcome
		if outcome == "" {
			outcome = "error"
		}
		metrics.RecordIntegration("hubspot", "account_details", "http", outcome, time.Since(started))
		receiptOutcome := result.Outcome
		if receiptOutcome == "" {
			receiptOutcome = "error"
		}
		receiptErr := integration.RecordResult(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "hubspot", Operation: "account_details", Channel: "http",
			Outcome: receiptOutcome, StatusClass: result.StatusClass, ErrorCode: result.ErrorCode,
			Elapsed: time.Since(started), QuotaRemaining: result.QuotaRemaining,
			RetryAfterMS: result.RetryAfterMS, AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		result.ReceiptStatus = integration.ReceiptStatus(receiptErr)
	}()

	token = strings.TrimSpace(token)
	if token == "" || len([]byte(token)) > maxProbeTokenLength || strings.IndexByte(token, '\n') >= 0 || strings.IndexByte(token, '\r') >= 0 {
		result = Result{Outcome: "error", ErrorCode: "invalid_config"}
		return result, errors.New("hubspot token is invalid")
	}
	base := strings.TrimRight(c.baseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	client := c.httpClient
	if client == nil {
		client = newHTTPClient()
	}
	requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, requestErr := http.NewRequestWithContext(requestCtx, http.MethodGet, base+accountDetailsPath, nil)
	if requestErr != nil {
		result = Result{Outcome: "error", ErrorCode: "invalid_config"}
		return result, errors.New("hubspot probe request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, requestErr := client.Do(req)
	if requestErr != nil {
		result = Result{Outcome: "error"}
		if errors.Is(requestErr, context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			result.ErrorCode = "timeout"
		} else if errors.Is(requestErr, context.Canceled) || errors.Is(requestCtx.Err(), context.Canceled) {
			result.ErrorCode = "canceled"
		} else {
			result.ErrorCode = "provider_error"
		}
		return result, errors.New("hubspot probe request failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	result.QuotaRemaining = quotaRemaining(resp.Header)
	result.RetryAfterMS = retryAfterMS(resp.Header.Get("Retry-After"))
	result.StatusClass = statusClass(resp.StatusCode)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.Outcome = "ok"
		result.ErrorCode = ""
		return result, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		result.Outcome = "error"
		result.ErrorCode = "auth_expired"
		return result, errors.New("hubspot authentication failed")
	case resp.StatusCode == http.StatusTooManyRequests:
		result.Outcome = "error"
		result.ErrorCode = "rate_limited"
		return result, errors.New("hubspot rate limit exceeded")
	default:
		result.Outcome = "error"
		result.ErrorCode = "provider_error"
		return result, fmt.Errorf("hubspot request returned %s", result.StatusClass)
	}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		// Provider credentials must never follow a redirect to an authority that
		// is outside this fixed adapter. Treat the response as a provider error
		// and let the bounded status classification handle it.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return ""
	}
	return strconv.Itoa(status/100) + "xx"
}

func quotaRemaining(header http.Header) *int64 {
	var values []int64
	for _, name := range []string{"X-HubSpot-RateLimit-Daily-Remaining", "X-HubSpot-RateLimit-Secondly-Remaining"} {
		value, err := strconv.ParseInt(strings.TrimSpace(header.Get(name)), 10, 64)
		if err == nil && value >= 0 && value <= maxQuotaRemaining {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return nil
	}
	minimum := values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return &minimum
}

func retryAfterMS(raw string) *int64 {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds < 0 {
		return nil
	}
	value := seconds * 1000
	if seconds > maxRetryAfterMS/1000 || value > maxRetryAfterMS {
		return nil
	}
	return &value
}
