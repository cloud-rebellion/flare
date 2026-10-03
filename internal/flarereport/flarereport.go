// Package flarereport wires Flare's own error reporting (dogfooding). It is
// the standard estate pattern plus a self-ingest loop guard: this service IS
// the ingest endpoint, so a panic in the ingest path would otherwise generate
// an event, whose delivery could panic again, and so on. BeforeSend caps
// self-reports at maxEventsPerMinute and drops the rest.
package flarereport

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

// maxEventsPerMinute bounds self-reported events so an error loop in the
// ingest path cannot self-amplify into unbounded writes.
const maxEventsPerMinute = 10

var sendGate struct {
	mu          sync.Mutex
	windowStart time.Time
	sent        int
	dropped     int
}

// allowSend admits at most maxEventsPerMinute events per fixed window and
// counts what it drops so the loss is visible in logs on window reset.
func allowSend() bool {
	sendGate.mu.Lock()
	defer sendGate.mu.Unlock()
	now := time.Now()
	if now.Sub(sendGate.windowStart) >= time.Minute {
		if sendGate.dropped > 0 {
			slog.Warn("flare: self-report loop guard dropped events", "dropped", sendGate.dropped)
		}
		sendGate.windowStart = now
		sendGate.sent = 0
		sendGate.dropped = 0
	}
	if sendGate.sent >= maxEventsPerMinute {
		sendGate.dropped++
		return false
	}
	sendGate.sent++
	return true
}

// InitFlare wires error reporting to the house Flare instance (Sentry-wire
// protocol) when FLARE_DSN is set in the environment. The DSN is injected by
// the Hephaestus flare-provision deploy step; without it this is a no-op so
// dev runs and self-hosts boot unchanged.
func InitFlare(service, release string) bool {
	dsn := os.Getenv("FLARE_DSN")
	if dsn == "" {
		return false
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Release:          release,
		Environment:      flareEnvironment(),
		ServerName:       service,
		EnableTracing:    true,
		TracesSampleRate: tracesSampleRate(),
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			if !allowSend() {
				return nil
			}
			return scrubRequestEvent(event)
		},
		// Transaction envelopes route through a SEPARATE hook. Registering only
		// BeforeSend covers errors and leaves every traced request unscrubbed,
		// which on this service is the higher-volume half.
		BeforeSendTransaction: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			if !allowSend() {
				return nil
			}
			return scrubRequestEvent(event)
		},
	})
	if err != nil {
		slog.Warn("flare: error reporting disabled (sentry init failed)", "error", err)
		return false
	}
	slog.Info("flare: error reporting enabled", "service", service)
	startHeartbeat(service)
	installLogShipper(service)
	return true
}

// flareEnvironment mirrors config.Load's ENVIRONMENT default so Flare's own
// startup and request signals retain an explicit environment dimension even
// when a local process has not set the variable. Keep this separate from the
// release: one immutable build may legitimately run in staging and production.
func flareEnvironment() string {
	if value := strings.TrimSpace(os.Getenv("ENVIRONMENT")); value != "" {
		return value
	}
	return "development"
}

// FlareRecoverer captures panics to Flare and re-panics so the existing
// recovery middleware (chi Recoverer) still renders the 500. Mount it AFTER
// Recoverer in the chain so it sees the panic first. Safe to mount when
// InitFlare was a no-op: capture calls on an uninitialized hub do nothing.
func FlareRecoverer(next http.Handler) http.Handler {
	traced := flareTracer(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				hub := sentry.CurrentHub().Clone()
				hub.Scope().SetRequest(r)
				hub.RecoverWithContext(r.Context(), rec)
				hub.Flush(2 * time.Second)
				panic(rec)
			}
		}()
		traced.ServeHTTP(w, r)
	})
}

// CaptureErr reports a non-panic error to Flare. No-op when reporting is
// disabled. Use for errors that are handled but should page someone.
func CaptureErr(err error) {
	if err == nil {
		return
	}
	sentry.CaptureException(err)
}

// Flush drains the SDK queue during graceful shutdown. A true result means
// the local queue drained before the deadline; it does not prove Flare's
// ingest worker durably accepted every envelope.
func Flush(timeout time.Duration) bool {
	return sentry.Flush(timeout)
}
