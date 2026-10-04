package flarereport

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	sentry "github.com/getsentry/sentry-go"
)

// tracesSampleRate is the fraction of requests traced, tunable per service via
// FLARE_TRACES_SAMPLE_RATE (0..1). Default 0.1. A value <= 0 disables tracing
// with zero per-request overhead.
func tracesSampleRate() float64 {
	if v := strings.TrimSpace(os.Getenv("FLARE_TRACES_SAMPLE_RATE")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return 0.1
}

// flareTracer wraps a handler in a Sentry transaction per request so the traces
// pillar populates from the same sentry-go SDK that already reports errors.
//
// Two deliberate safety choices, both learned the hard way:
//   - It wraps the ResponseWriter only with a forwarding writer that preserves
//     the streaming interfaces used by HTTP handlers. A wrapper that dropped
//     http.Flusher/Hijacker or io.ReaderFrom would break SSE, upgrades, and
//     streaming responses estate-wide.
//   - isUntraceable skips Flare's own ingest routes. Tracing an ingest request
//     would emit a transaction envelope that is itself an ingest request, which
//     would be traced again: an unbounded self-amplification loop.
//
// No-op (returns next unchanged) when tracing is disabled.
func flareTracer(next http.Handler) http.Handler {
	if tracesSampleRate() <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUntraceable(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		txn := sentry.StartTransaction(r.Context(),
			r.Method+" "+normalizeTracePath(r.URL.Path),
			sentry.ContinueFromRequest(r),
			sentry.WithOpName("http.server"),
			sentry.WithTransactionSource(sentry.SourceURL),
		)
		base := &tracingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		rw := wrapTracingResponseWriter(base, w)
		defer func() {
			// The standard-library Sentry integration records the final response
			// status on the transaction. Keep the same bounded, code-only
			// evidence here so sampled backend requests can be joined with the
			// HTTP outcome without storing headers, URLs, or response bodies.
			recordHTTPResponseStatus(txn, base.status)
			txn.Finish()
		}()
		next.ServeHTTP(rw, r.WithContext(txn.Context()))
	})
}

func recordHTTPResponseStatus(txn *sentry.Span, status int) {
	// net/http permits three-digit extension codes, but Flare only retains the
	// standards-defined 1xx--5xx range in transaction evidence. Treat an
	// invalid or extension code as an internal error rather than persisting an
	// unbounded value from a custom ResponseWriter.
	if status < 100 || status >= 600 {
		status = http.StatusInternalServerError
	}
	txn.Status = sentry.HTTPtoSpanStatus(status)
	txn.SetData("http.response.status_code", status)
}

// tracingResponseWriter records only the final HTTP status while forwarding
// the optional interfaces that are meaningful for a server response. The
// method set intentionally mirrors the established metrics response writer so
// tracing can be added anywhere in a middleware stack without changing handler
// behavior. Unwrap also lets http.ResponseController reach the underlying
// writer on newer Go versions.
type tracingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *tracingResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *tracingResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *tracingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *tracingResponseWriter) flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *tracingResponseWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("response writer does not support hijacking")
}

func (w *tracingResponseWriter) push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *tracingResponseWriter) readFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

// wrapTracingResponseWriter returns a forwarding wrapper with exactly the
// optional interfaces supported by the underlying writer. Exposing a
// Hijacker, Flusher, Pusher, or ReaderFrom method when the underlying writer
// lacks it changes handler feature detection, so the combinations are kept
// explicit instead of advertising unsupported capabilities with a runtime
// error. The base pointer is separate from the interface so the tracer can
// read the final status after the handler returns.
func wrapTracingResponseWriter(base *tracingResponseWriter, underlying http.ResponseWriter) http.ResponseWriter {
	var mask uint8
	if _, ok := underlying.(http.Flusher); ok {
		mask |= 1
	}
	if _, ok := underlying.(http.Hijacker); ok {
		mask |= 2
	}
	if _, ok := underlying.(http.Pusher); ok {
		mask |= 4
	}
	if _, ok := underlying.(io.ReaderFrom); ok {
		mask |= 8
	}
	fc := tracingFlusherCapability{rw: base}
	hc := tracingHijackerCapability{rw: base}
	pc := tracingPusherCapability{rw: base}
	rc := tracingReaderFromCapability{rw: base}
	switch mask {
	case 0:
		return base
	case 1:
		return &tracingResponseWriterF{tracingResponseWriter: base, tracingFlusherCapability: fc}
	case 2:
		return &tracingResponseWriterH{tracingResponseWriter: base, tracingHijackerCapability: hc}
	case 3:
		return &tracingResponseWriterFH{tracingResponseWriter: base, tracingFlusherCapability: fc, tracingHijackerCapability: hc}
	case 4:
		return &tracingResponseWriterP{tracingResponseWriter: base, tracingPusherCapability: pc}
	case 5:
		return &tracingResponseWriterFP{tracingResponseWriter: base, tracingFlusherCapability: fc, tracingPusherCapability: pc}
	case 6:
		return &tracingResponseWriterHP{tracingResponseWriter: base, tracingHijackerCapability: hc, tracingPusherCapability: pc}
	case 7:
		return &tracingResponseWriterFHP{tracingResponseWriter: base, tracingFlusherCapability: fc, tracingHijackerCapability: hc, tracingPusherCapability: pc}
	case 8:
		return &tracingResponseWriterR{tracingResponseWriter: base, tracingReaderFromCapability: rc}
	case 9:
		return &tracingResponseWriterFR{tracingResponseWriter: base, tracingFlusherCapability: fc, tracingReaderFromCapability: rc}
	case 10:
		return &tracingResponseWriterHR{tracingResponseWriter: base, tracingHijackerCapability: hc, tracingReaderFromCapability: rc}
	case 11:
		return &tracingResponseWriterFHR{tracingResponseWriter: base, tracingFlusherCapability: fc, tracingHijackerCapability: hc, tracingReaderFromCapability: rc}
	case 12:
		return &tracingResponseWriterPR{tracingResponseWriter: base, tracingPusherCapability: pc, tracingReaderFromCapability: rc}
	case 13:
		return &tracingResponseWriterFPR{tracingResponseWriter: base, tracingFlusherCapability: fc, tracingPusherCapability: pc, tracingReaderFromCapability: rc}
	case 14:
		return &tracingResponseWriterHPR{tracingResponseWriter: base, tracingHijackerCapability: hc, tracingPusherCapability: pc, tracingReaderFromCapability: rc}
	default:
		return &tracingResponseWriterFHPR{tracingResponseWriter: base, tracingFlusherCapability: fc, tracingHijackerCapability: hc, tracingPusherCapability: pc, tracingReaderFromCapability: rc}
	}
}

type tracingFlusherCapability struct{ rw *tracingResponseWriter }

func (c tracingFlusherCapability) Flush() { c.rw.flush() }

type tracingHijackerCapability struct{ rw *tracingResponseWriter }

func (c tracingHijackerCapability) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return c.rw.hijack()
}

type tracingPusherCapability struct{ rw *tracingResponseWriter }

func (c tracingPusherCapability) Push(target string, opts *http.PushOptions) error {
	return c.rw.push(target, opts)
}

type tracingReaderFromCapability struct{ rw *tracingResponseWriter }

func (c tracingReaderFromCapability) ReadFrom(r io.Reader) (int64, error) {
	return c.rw.readFrom(r)
}

type tracingResponseWriterF struct {
	*tracingResponseWriter
	tracingFlusherCapability
}
type tracingResponseWriterH struct {
	*tracingResponseWriter
	tracingHijackerCapability
}
type tracingResponseWriterFH struct {
	*tracingResponseWriter
	tracingFlusherCapability
	tracingHijackerCapability
}
type tracingResponseWriterP struct {
	*tracingResponseWriter
	tracingPusherCapability
}
type tracingResponseWriterFP struct {
	*tracingResponseWriter
	tracingFlusherCapability
	tracingPusherCapability
}
type tracingResponseWriterHP struct {
	*tracingResponseWriter
	tracingHijackerCapability
	tracingPusherCapability
}
type tracingResponseWriterFHP struct {
	*tracingResponseWriter
	tracingFlusherCapability
	tracingHijackerCapability
	tracingPusherCapability
}
type tracingResponseWriterR struct {
	*tracingResponseWriter
	tracingReaderFromCapability
}
type tracingResponseWriterFR struct {
	*tracingResponseWriter
	tracingFlusherCapability
	tracingReaderFromCapability
}
type tracingResponseWriterHR struct {
	*tracingResponseWriter
	tracingHijackerCapability
	tracingReaderFromCapability
}
type tracingResponseWriterFHR struct {
	*tracingResponseWriter
	tracingFlusherCapability
	tracingHijackerCapability
	tracingReaderFromCapability
}
type tracingResponseWriterPR struct {
	*tracingResponseWriter
	tracingPusherCapability
	tracingReaderFromCapability
}
type tracingResponseWriterFPR struct {
	*tracingResponseWriter
	tracingFlusherCapability
	tracingPusherCapability
	tracingReaderFromCapability
}
type tracingResponseWriterHPR struct {
	*tracingResponseWriter
	tracingHijackerCapability
	tracingPusherCapability
	tracingReaderFromCapability
}
type tracingResponseWriterFHPR struct {
	*tracingResponseWriter
	tracingFlusherCapability
	tracingHijackerCapability
	tracingPusherCapability
	tracingReaderFromCapability
}

// isUntraceable skips health/infra/static noise and, crucially, Flare's own
// ingest routes (see flareTracer). No non-Flare service serves these paths, so
// skipping them everywhere is harmless.
func isUntraceable(p string) bool {
	switch p {
	case "/health", "/healthz", "/readyz", "/livez", "/ready", "/metrics",
		"/favicon.ico", "/robots.txt", "/store", "/store/":
		return true
	}
	if strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/assets/") ||
		strings.HasPrefix(p, "/_app/") || strings.HasPrefix(p, "/otlp") {
		return true
	}
	// Flare ingest under /api/{id}/{resource}: skip to break the ingest
	// self-trace loop. Keep the project and resource as distinct path segments
	// so a normal API route that merely ends in an ingest-looking word does not
	// get silently dropped from tracing. Check-ins have one additional slug
	// segment, so they are handled explicitly below.
	if !strings.HasPrefix(p, "/api/") {
		return false
	}
	// Accept one optional trailing slash, matching the registered routes. Do
	// not trim an arbitrary run of slashes: malformed or nested paths should
	// remain traceable so this guard cannot hide application traffic.
	trimmed := strings.TrimSuffix(p, "/")
	parts := strings.Split(strings.TrimPrefix(trimmed, "/api/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	switch parts[1] {
	case "envelope", "store", "events", "logs", "traces", "metrics", "security", "connector-attempts", "business-checks":
		return len(parts) == 2
	case "checkins":
		return len(parts) == 3 && parts[2] != ""
	default:
		return false
	}
}

// normalizeTracePath collapses high-cardinality segments (numeric ids, uuids,
// long cuid/hex tokens) to ":id" so transaction names stay low-cardinality,
// without coupling this shared package to any specific router.
func normalizeTracePath(p string) string {
	if p == "" || p == "/" {
		return p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if idLike(s) {
			segs[i] = ":id"
		}
	}
	return strings.Join(segs, "/")
}

func idLike(s string) bool {
	if s == "" {
		return false
	}
	digits, token, hasDigit := true, true, false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' || c == '_':
			digits = false
		default:
			digits, token = false, false
		}
	}
	if digits {
		return true // purely numeric id
	}
	// uuid / cuid / opaque token: id-charset, long, and carrying a digit.
	return token && hasDigit && len(s) >= 20
}
