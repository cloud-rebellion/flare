// Package integration provides the narrow, secret-free boundary used by
// outbound connectors to report durable health receipts. It deliberately has
// no database dependency: the API process installs the recorder, while AI,
// OIDC, GitHub, and alert packages only emit fixed fields.
package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

type scopeKey struct{}

// Scope identifies the tenant and (when applicable) project that initiated a
// connector call. Empty project IDs are valid for org-level connectors.
type Scope struct {
	OrgID        string
	ProjectID    string
	ConnectionID string
}

// WithScope attaches tenant provenance to a connector context. It carries no
// secrets and is intentionally separate from the API package's auth context.
func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

func ScopeFrom(ctx context.Context) Scope {
	scope, _ := ctx.Value(scopeKey{}).(Scope)
	return scope
}

// Attempt is the bounded, payload-free receipt shape. Provider and operation
// are fixed vocabulary values at each call boundary; channel is e.g. http or
// the configured alert channel type, never a URL or credential.
type Attempt struct {
	Scope       Scope
	Provider    string
	Operation   string
	Channel     string
	Outcome     string
	StatusClass string
	ErrorCode   string
	Elapsed     time.Duration
	// Provider hints are optional, bounded integers. They let an internal
	// connector retain the same quota/rate-limit/synchronization observations
	// as the public connector-attempt ingest route without carrying a provider
	// response, URL, or credential into the receipt. Values are milliseconds
	// except quota_remaining, which is a provider-defined count.
	QuotaRemaining *int64
	RetryAfterMS   *int64
	SyncLagMS      *int64
	AttemptedAt    time.Time
	CompletedAt    time.Time
}

type Recorder func(context.Context, Attempt)

var recorder struct {
	sync.RWMutex
	fn Recorder
}

// SetRecorder installs the process-local persistence hook. A nil recorder is
// valid in package tests and before the API server is constructed.
func SetRecorder(fn Recorder) {
	recorder.Lock()
	recorder.fn = fn
	recorder.Unlock()
}

// Record forwards a bounded receipt to the installed recorder. Calls without
// an authenticated org scope are intentionally dropped; there is no safe
// tenant to which they could be attributed.
func Record(ctx context.Context, attempt Attempt) {
	if attempt.Scope.OrgID == "" {
		attempt.Scope = ScopeFrom(ctx)
	}
	if attempt.Scope.OrgID == "" || attempt.Provider == "" || attempt.Operation == "" || attempt.Channel == "" {
		return
	}
	attempt.Provider = bound(attempt.Provider, 32)
	attempt.Operation = bound(attempt.Operation, 64)
	attempt.Channel = bound(attempt.Channel, 32)
	attempt.Outcome, attempt.StatusClass, attempt.ErrorCode = canonicalOutcome(attempt.Outcome, attempt.StatusClass, attempt.ErrorCode)
	if attempt.Elapsed < 0 {
		attempt.Elapsed = 0
	}
	// Keep the process-local integration boundary subject to the same bounds as
	// the authenticated public connector route. Invalid hints are omitted so a
	// provider adapter cannot make a durable receipt write fail, and no value is
	// silently clamped into a misleading observation.
	attempt.QuotaRemaining = boundedHint(attempt.QuotaRemaining, 1_000_000_000_000_000)
	attempt.RetryAfterMS = boundedHint(attempt.RetryAfterMS, 604_800_000)
	attempt.SyncLagMS = boundedHint(attempt.SyncLagMS, 604_800_000)
	if attempt.AttemptedAt.IsZero() {
		attempt.AttemptedAt = time.Now().UTC().Add(-attempt.Elapsed)
	}
	if attempt.CompletedAt.IsZero() {
		attempt.CompletedAt = time.Now().UTC()
	}
	recorder.RLock()
	fn := recorder.fn
	recorder.RUnlock()
	if fn != nil {
		fn(ctx, attempt)
	}
}

func boundedHint(value *int64, max int64) *int64 {
	if value == nil || *value < 0 || *value > max {
		return nil
	}
	v := *value
	return &v
}

// ClassifyError maps provider errors to fixed, safe labels. Error text is never
// persisted because it may contain URLs, response bodies, or credentials.
func ClassifyError(err error) (outcome, statusClass, errorCode string) {
	if err == nil {
		return "ok", "2xx", ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "error", "", "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "error", "", "canceled"
	}
	// Connectors use stable phrases for HTTP failures. Matching only the
	// status-class token records useful health detail without storing provider
	// response text.
	lower := strings.ToLower(err.Error())
	for _, c := range []string{"4", "5"} {
		if strings.Contains(lower, "status "+c) || strings.Contains(lower, "returned "+c) {
			return "error", c + "xx", "http_" + c + "xx"
		}
	}
	if strings.Contains(lower, "missing") || strings.Contains(lower, "invalid") || strings.Contains(lower, "misconfigured") {
		return "error", "", "invalid_config"
	}
	return "error", "", "provider_error"
}

func canonicalOutcome(outcome, statusClass, errorCode string) (string, string, string) {
	if outcome == "ok" {
		return "ok", "2xx", ""
	}
	if outcome != "error" {
		outcome = "error"
	}
	if statusClass == "" && errorCode == "" {
		errorCode = "provider_error"
	}
	return outcome, bound(statusClass, 16), bound(errorCode, 64)
}

func bound(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
