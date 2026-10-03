package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bright-interaction/flare/internal/connectorcheck"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/metrics"
)

const (
	defaultHealthBaseURL = "https://api.github.com"
	healthPath           = "/user"
	healthOperation      = "user"
	healthProbeTimeout   = 12 * time.Second
	maxHealthBodyBytes   = 64 << 10
	maxHealthTokenBytes  = 16 << 10
	maxHealthQuota       = int64(1_000_000_000_000_000)
	maxHealthRetryAfter  = int64(7 * 24 * 60 * 60 * 1000)
)

// HealthClient checks one GitHub installation with the read-only /user
// endpoint. The authority is fixed in production; the private fields exist
// only so package tests can inject a transport without opening a listener.
type HealthClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewHealthClient returns the fixed-authority GitHub health adapter.
func NewHealthClient() HealthClient {
	return HealthClient{baseURL: defaultHealthBaseURL, httpClient: newHealthHTTPClient()}
}

// Check performs a bounded authentication check. Provider response bodies and
// account details are discarded; only fixed outcome/error codes and safe rate
// limit hints are returned or recorded.
func (c HealthClient) Check(ctx context.Context, token string) (result connectorcheck.Result, err error) {
	startedAt := time.Now().UTC()
	started := time.Now()
	defer func() {
		outcome := result.Outcome
		if outcome == "" {
			outcome = "error"
		}
		metrics.RecordIntegration("github", healthOperation, "http", outcome, time.Since(started))
		receiptOutcome := result.Outcome
		if receiptOutcome == "" {
			receiptOutcome = "error"
		}
		receiptErr := integration.RecordResult(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "github", Operation: healthOperation, Channel: "http",
			Outcome: receiptOutcome, StatusClass: result.StatusClass, ErrorCode: result.ErrorCode,
			Elapsed: time.Since(started), QuotaRemaining: result.QuotaRemaining,
			RetryAfterMS: result.RetryAfterMS, AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		result.ReceiptStatus = integration.ReceiptStatus(receiptErr)
	}()

	token = strings.TrimSpace(token)
	if token == "" || len([]byte(token)) > maxHealthTokenBytes || strings.IndexFunc(token, unicode.IsControl) >= 0 {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: "invalid_config"}
		return result, errors.New("github token is invalid")
	}
	base := strings.TrimRight(c.baseURL, "/")
	if base == "" {
		base = defaultHealthBaseURL
	}
	client := c.httpClient
	if client == nil {
		client = newHealthHTTPClient()
	}
	requestCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	req, requestErr := http.NewRequestWithContext(requestCtx, http.MethodGet, base+healthPath, nil)
	if requestErr != nil {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: "invalid_config"}
		return result, errors.New("github probe request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, requestErr := client.Do(req)
	if requestErr != nil {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: healthRequestErrorCode(requestCtx, requestErr)}
		return result, errors.New("github probe request failed")
	}
	defer resp.Body.Close()
	if _, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxHealthBodyBytes)); readErr != nil {
		result.Outcome, result.ErrorCode = "error", "provider_error"
		return result, errors.New("github probe response could not be read")
	}
	result.StatusClass = healthStatusClass(resp.StatusCode)
	result.QuotaRemaining = healthQuotaRemaining(resp.Header.Get("X-RateLimit-Remaining"))
	result.RetryAfterMS = healthRetryAfterMS(resp.Header.Get("Retry-After"))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.Outcome = "ok"
		result.ErrorCode = ""
		return result, nil
	case resp.StatusCode == http.StatusUnauthorized:
		result.Outcome, result.ErrorCode = "error", "auth_expired"
		return result, errors.New("github authentication failed")
	case resp.StatusCode == http.StatusForbidden && result.QuotaRemaining != nil && *result.QuotaRemaining == 0:
		result.Outcome, result.ErrorCode = "error", "rate_limited"
		return result, errors.New("github rate limit exceeded")
	case resp.StatusCode == http.StatusForbidden:
		result.Outcome, result.ErrorCode = "error", "permission_denied"
		return result, errors.New("github permission denied")
	case resp.StatusCode == http.StatusTooManyRequests:
		result.Outcome, result.ErrorCode = "error", "rate_limited"
		return result, errors.New("github rate limit exceeded")
	default:
		result.Outcome, result.ErrorCode = "error", "provider_error"
		return result, fmt.Errorf("github request returned %s", result.StatusClass)
	}
}

func newHealthHTTPClient() *http.Client {
	return &http.Client{
		Timeout: healthProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func healthRequestErrorCode(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "canceled"
	}
	return "provider_error"
}

func healthStatusClass(status int) string {
	if status < 100 || status > 599 {
		return ""
	}
	return strconv.Itoa(status/100) + "xx"
}

func healthQuotaRemaining(raw string) *int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 || value > maxHealthQuota {
		return nil
	}
	return &value
}

func healthRetryAfterMS(raw string) *int64 {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds < 0 || seconds > maxHealthRetryAfter/1000 {
		return nil
	}
	value := seconds * 1000
	return &value
}
