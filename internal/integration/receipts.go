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

// ResultRecorder is the persistence boundary for a connector receipt. The
// callback receives only the canonical, payload-free Attempt and returns the
// durable write error, if any. Provider adapters can use RecordResult when
// they need to observe persistence failure without changing the provider
// result they return to their caller.
type ResultRecorder func(context.Context, Attempt) error

var (
	// ErrRecorderUnavailable means the process has no installed durable receipt
	// recorder. It is intentionally distinct from provider errors: callers may
	// retry the receipt boundary without retrying the provider request.
	ErrRecorderUnavailable = errors.New("connector receipt recorder unavailable")
	// ErrUnscopedAttempt means the attempt did not carry tenant provenance.
	// Record keeps the historical drop behavior for compatibility, while
	// RecordResult exposes the reason to callers that require durable evidence.
	ErrUnscopedAttempt = errors.New("connector receipt requires an organization scope")
	// ErrInvalidAttempt means a required fixed receipt field was absent.
	ErrInvalidAttempt = errors.New("connector receipt is missing required fields")
)

var recorder struct {
	sync.RWMutex
	fn       Recorder
	resultFn ResultRecorder
}

// SetRecorder installs the process-local persistence hook. A nil recorder is
// valid in package tests and before the API server is constructed.
func SetRecorder(fn Recorder) {
	recorder.Lock()
	recorder.fn = fn
	recorder.resultFn = nil
	recorder.Unlock()
}

// SetRecorderWithError installs a recorder that reports durable persistence
// errors. It replaces the compatibility recorder installed by SetRecorder.
// The returned error never contains provider response text or credentials;
// the recorder owns the safe error classification at its storage boundary.
func SetRecorderWithError(fn ResultRecorder) {
	recorder.Lock()
	recorder.fn = nil
	recorder.resultFn = fn
	recorder.Unlock()
}

// Record forwards a bounded receipt to the installed recorder. Calls without
// an authenticated org scope are intentionally dropped; there is no safe
// tenant to which they could be attributed.
func Record(ctx context.Context, attempt Attempt) {
	_ = RecordResult(ctx, attempt)
}

// RecordResult forwards a bounded receipt and reports whether the durable
// recorder accepted it. It deliberately does not retry the provider call and
// does not mutate Attempt based on the recorder's error. Callers that preserve
// provider semantics can ignore the return value; callers that need to gate a
// separate acceptance operation can return or surface it independently.
func RecordResult(ctx context.Context, attempt Attempt) error {
	if attempt.Scope.OrgID == "" {
		attempt.Scope = ScopeFrom(ctx)
	}
	if attempt.Scope.OrgID == "" {
		return ErrUnscopedAttempt
	}
	if attempt.Provider == "" || attempt.Operation == "" || attempt.Channel == "" {
		return ErrInvalidAttempt
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
	resultFn := recorder.resultFn
	recorder.RUnlock()
	if resultFn != nil {
		return resultFn(ctx, attempt)
	}
	if fn != nil {
		fn(ctx, attempt)
	}
	if resultFn == nil && fn == nil {
		return ErrRecorderUnavailable
	}
	return nil
}

// ReceiptStatus reduces a recorder result to a fixed public value. Database
// errors and provider text never cross this boundary.
func ReceiptStatus(err error) string {
	switch {
	case err == nil:
		return "accepted"
	case errors.Is(err, ErrUnscopedAttempt):
		return "unscoped"
	case errors.Is(err, ErrInvalidAttempt):
		return "invalid"
	default:
		return "unavailable"
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
