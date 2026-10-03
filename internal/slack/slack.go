// Package slack contains the bounded health probe for a Slack installation.
// It calls auth.test only; workspace content and identity details never enter
// Flare's connector receipts.
package slack

import (
	"context"
	"encoding/json"
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
	defaultBaseURL      = "https://slack.com"
	authTestPath        = "/api/auth.test"
	probeOperation      = "auth.test"
	probeTimeout        = 12 * time.Second
	maxResponseBytes    = 64 << 10
	maxProbeTokenLength = 16 << 10
	maxRetryAfterMS     = int64(7 * 24 * 60 * 60 * 1000)
)

// Client probes one fixed Slack authority. The private fields let package
// tests inject a transport without making production authority configurable.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New returns the production Slack client. The provider authority is fixed to
// slack.com; credentials are supplied only per call.
func New() Client {
	return Client{baseURL: defaultBaseURL, httpClient: newHTTPClient()}
}

// Check performs Slack's read-only auth.test request. It keeps only a boolean
// success result, a fixed error code, status class, and bounded Retry-After.
func (c Client) Check(ctx context.Context, token string) (result connectorcheck.Result, err error) {
	startedAt := time.Now().UTC()
	started := time.Now()
	defer func() {
		outcome := result.Outcome
		if outcome == "" {
			outcome = "error"
		}
		metrics.RecordIntegration("slack", probeOperation, "http", outcome, time.Since(started))
		receiptOutcome := result.Outcome
		if receiptOutcome == "" {
			receiptOutcome = "error"
		}
		receiptErr := integration.RecordResult(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "slack", Operation: probeOperation, Channel: "http",
			Outcome: receiptOutcome, StatusClass: result.StatusClass, ErrorCode: result.ErrorCode,
			Elapsed: time.Since(started), RetryAfterMS: result.RetryAfterMS,
			AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		result.ReceiptStatus = integration.ReceiptStatus(receiptErr)
	}()

	token = strings.TrimSpace(token)
	if token == "" || len([]byte(token)) > maxProbeTokenLength || strings.IndexFunc(token, unicode.IsControl) >= 0 {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: "invalid_config"}
		return result, errors.New("slack token is invalid")
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
	req, requestErr := http.NewRequestWithContext(requestCtx, http.MethodGet, base+authTestPath, nil)
	if requestErr != nil {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: "invalid_config"}
		return result, errors.New("slack probe request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, requestErr := client.Do(req)
	if requestErr != nil {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: requestErrorCode(requestCtx, requestErr)}
		return result, errors.New("slack probe request failed")
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	result.StatusClass = statusClass(resp.StatusCode)
	result.RetryAfterMS = retryAfterMS(resp.Header.Get("Retry-After"))
	if readErr != nil {
		result.Outcome, result.ErrorCode = "error", "provider_error"
		return result, errors.New("slack probe response could not be read")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		result.Outcome, result.ErrorCode = "error", "rate_limited"
		return result, errors.New("slack rate limit exceeded")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Outcome, result.ErrorCode = "error", "auth_expired"
		return result, errors.New("slack authentication failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Outcome, result.ErrorCode = "error", "provider_error"
		return result, fmt.Errorf("slack request returned %s", result.StatusClass)
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		result.Outcome, result.ErrorCode = "error", "provider_error"
		return result, errors.New("slack probe returned invalid JSON")
	}
	if !envelope.OK {
		result.Outcome = "error"
		switch envelope.Error {
		case "invalid_auth", "token_revoked", "account_inactive", "not_authed":
			result.ErrorCode = "auth_expired"
		case "missing_scope", "permission_denied":
			result.ErrorCode = "permission_denied"
		case "ratelimited":
			result.ErrorCode = "rate_limited"
		default:
			result.ErrorCode = "provider_error"
		}
		return result, errors.New("slack auth check failed")
	}
	result.Outcome = "ok"
	result.ErrorCode = ""
	return result, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func requestErrorCode(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "canceled"
	}
	return "provider_error"
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return ""
	}
	return strconv.Itoa(status/100) + "xx"
}

func retryAfterMS(raw string) *int64 {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds < 0 || seconds > maxRetryAfterMS/1000 {
		return nil
	}
	value := seconds * 1000
	return &value
}
