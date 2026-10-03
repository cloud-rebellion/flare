// Package openai contains the bounded health probe for an OpenAI API key. It
// calls the models endpoint only; model metadata and response bodies never
// enter Flare's connector receipts.
package openai

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
	defaultBaseURL      = "https://api.openai.com"
	modelsPath          = "/v1/models"
	probeOperation      = "models"
	probeTimeout        = 12 * time.Second
	maxResponseBytes    = 64 << 10
	maxProbeTokenLength = 16 << 10
	maxQuotaRemaining   = int64(1_000_000_000_000_000)
	maxRetryAfterMS     = int64(7 * 24 * 60 * 60 * 1000)
)

// Client probes one fixed OpenAI authority. The private fields let package
// tests inject a transport without making production authority configurable.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New returns the production OpenAI client. The provider authority is fixed to
// api.openai.com; credentials are supplied only per call.
func New() Client {
	return Client{baseURL: defaultBaseURL, httpClient: newHTTPClient()}
}

// Check performs a bounded read-only models request. The response body is
// drained and discarded; only status, fixed codes, and safe rate-limit hints
// are returned.
func (c Client) Check(ctx context.Context, token string) (result connectorcheck.Result, err error) {
	startedAt := time.Now().UTC()
	started := time.Now()
	defer func() {
		outcome := result.Outcome
		if outcome == "" {
			outcome = "error"
		}
		metrics.RecordIntegration("openai", probeOperation, "http", outcome, time.Since(started))
		receiptOutcome := result.Outcome
		if receiptOutcome == "" {
			receiptOutcome = "error"
		}
		receiptErr := integration.RecordResult(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "openai", Operation: probeOperation, Channel: "http",
			Outcome: receiptOutcome, StatusClass: result.StatusClass, ErrorCode: result.ErrorCode,
			Elapsed: time.Since(started), QuotaRemaining: result.QuotaRemaining,
			RetryAfterMS: result.RetryAfterMS, AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		result.ReceiptStatus = integration.ReceiptStatus(receiptErr)
	}()

	token = strings.TrimSpace(token)
	if token == "" || len([]byte(token)) > maxProbeTokenLength || strings.IndexFunc(token, unicode.IsControl) >= 0 {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: "invalid_config"}
		return result, errors.New("openai token is invalid")
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
	req, requestErr := http.NewRequestWithContext(requestCtx, http.MethodGet, base+modelsPath, nil)
	if requestErr != nil {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: "invalid_config"}
		return result, errors.New("openai probe request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, requestErr := client.Do(req)
	if requestErr != nil {
		result = connectorcheck.Result{Outcome: "error", ErrorCode: requestErrorCode(requestCtx, requestErr)}
		return result, errors.New("openai probe request failed")
	}
	defer resp.Body.Close()
	if _, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes)); readErr != nil {
		result.Outcome, result.ErrorCode = "error", "provider_error"
		return result, errors.New("openai probe response could not be read")
	}
	result.StatusClass = statusClass(resp.StatusCode)
	result.QuotaRemaining = quotaRemaining(resp.Header.Get("x-ratelimit-remaining-requests"))
	result.RetryAfterMS = retryAfterMS(resp.Header.Get("Retry-After"))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.Outcome = "ok"
		result.ErrorCode = ""
		return result, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		result.Outcome = "error"
		result.ErrorCode = "auth_expired"
		return result, errors.New("openai authentication failed")
	case resp.StatusCode == http.StatusTooManyRequests:
		result.Outcome = "error"
		result.ErrorCode = "rate_limited"
		return result, errors.New("openai rate limit exceeded")
	default:
		result.Outcome = "error"
		result.ErrorCode = "provider_error"
		return result, fmt.Errorf("openai request returned %s", result.StatusClass)
	}
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

func quotaRemaining(raw string) *int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 || value > maxQuotaRemaining {
		return nil
	}
	return &value
}

func retryAfterMS(raw string) *int64 {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds < 0 || seconds > maxRetryAfterMS/1000 {
		return nil
	}
	value := seconds * 1000
	return &value
}
