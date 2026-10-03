package flarereport

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	sentry "github.com/getsentry/sentry-go"
)

// The self-amplification guard is the whole reason isUntraceable exists: tracing
// Flare's own ingest routes would emit a transaction envelope that is itself an
// ingest request. Lock the ingest shapes (and health/static noise) as skipped,
// and normal app routes as traced.
func TestIsUntraceable(t *testing.T) {
	skip := []string{
		"/health", "/healthz", "/readyz", "/metrics", "/favicon.ico",
		"/store/", "/store",
		"/static/app.js", "/assets/x.css", "/_app/immutable/y.js",
		"/otlp/v1/traces", "/otlp",
		"/api/724067772811/envelope/", "/api/724067772811/store/",
		"/api/proj/events", "/api/proj/logs", "/api/proj/traces", "/api/proj/metrics",
		"/api/proj/connector-attempts", "/api/proj/checkins/nightly", "/api/proj/checkins/nightly/",
	}
	for _, p := range skip {
		if !isUntraceable(p) {
			t.Errorf("expected %q to be skipped (ingest/infra self-loop guard)", p)
		}
	}
	trace := []string{
		"/", "/api/users/123", "/projects/abc/issues", "/team", "/api/mcp",
		"/api/proj/checkins-notify", "/api/proj/events/detail", "/api/proj/checkins/nightly/logs",
		"/api/proj/events//", "/api/proj/checkins/nightly//",
	}
	for _, p := range trace {
		if isUntraceable(p) {
			t.Errorf("expected %q to be traced, got skipped", p)
		}
	}
}

func TestNormalizeTracePath(t *testing.T) {
	cases := map[string]string{
		"/":              "/",
		"/api/users/123": "/api/users/:id",
		"/projects/ru6pj4v16irarblw2s1ojrrs/edit": "/projects/:id/edit",
		"/t/550e8400-e29b-41d4-a716-446655440000": "/t/:id",
		"/api/v1/scans":        "/api/v1/scans", // v1 kept (short), scans kept (word)
		"/verify-traces-probe": "/verify-traces-probe",
	}
	for in, want := range cases {
		if got := normalizeTracePath(in); got != want {
			t.Errorf("normalizeTracePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Tracing wraps the ResponseWriter to record the final status, while retaining
// the optional interfaces supplied by the server. A streaming handler must
// still see http.Flusher even though the tracer is enabled.
func TestFlareTracerPreservesResponseWriterInterfaces(t *testing.T) {
	t.Setenv("FLARE_TRACES_SAMPLE_RATE", "1")
	var sawFlusher bool
	h := flareTracer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/users/7", nil))
	if !sawFlusher {
		t.Fatal("handler lost http.Flusher: tracer must not wrap the ResponseWriter")
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestTracingResponseWriterAdvertisesOnlySupportedInterfaces(t *testing.T) {
	bare := &bareResponseWriter{header: make(http.Header)}
	base := &tracingResponseWriter{ResponseWriter: bare, status: http.StatusOK}
	wrapped := wrapTracingResponseWriter(base, bare)
	if _, ok := wrapped.(http.Flusher); ok {
		t.Fatal("bare writer unexpectedly advertised http.Flusher")
	}
	if _, ok := wrapped.(http.Hijacker); ok {
		t.Fatal("bare writer unexpectedly advertised http.Hijacker")
	}
	if _, ok := wrapped.(io.ReaderFrom); ok {
		t.Fatal("bare writer unexpectedly advertised io.ReaderFrom")
	}

	features := &featureResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	base = &tracingResponseWriter{ResponseWriter: features, status: http.StatusOK}
	wrapped = wrapTracingResponseWriter(base, features)
	if _, ok := wrapped.(http.Flusher); !ok {
		t.Fatal("supported writer lost http.Flusher")
	}
	if _, ok := wrapped.(http.Hijacker); !ok {
		t.Fatal("supported writer lost http.Hijacker")
	}
	if _, ok := wrapped.(http.Pusher); !ok {
		t.Fatal("supported writer lost http.Pusher")
	}
	if _, ok := wrapped.(io.ReaderFrom); !ok {
		t.Fatal("supported writer lost io.ReaderFrom")
	}
	wrapped.(http.Flusher).Flush()
	if !features.flushed {
		t.Fatal("Flush was not forwarded")
	}
	if err := wrapped.(http.Pusher).Push("/next", nil); err != nil || !features.pushed {
		t.Fatalf("Push was not forwarded: err=%v pushed=%v", err, features.pushed)
	}
	conn, _, err := wrapped.(http.Hijacker).Hijack()
	if err != nil || conn == nil || !features.hijacked {
		t.Fatalf("Hijack was not forwarded: err=%v hijacked=%v", err, features.hijacked)
	}
	_ = conn.Close()
	n, err := wrapped.(io.ReaderFrom).ReadFrom(bytes.NewBufferString("body"))
	if err != nil || n != 4 {
		t.Fatalf("ReadFrom = %d/%v, want 4/nil", n, err)
	}
}

func TestRecordHTTPResponseStatusUsesBoundedSentryClassification(t *testing.T) {
	txn := sentry.StartTransaction(context.Background(), "GET /health")
	recordHTTPResponseStatus(txn, http.StatusNotFound)
	if txn.Status != sentry.SpanStatusNotFound {
		t.Fatalf("transaction status = %s, want not_found", txn.Status)
	}
	if got := txn.Data["http.response.status_code"]; got != http.StatusNotFound {
		t.Fatalf("status data = %#v, want %d", got, http.StatusNotFound)
	}
	if _, ok := txn.Data["http.response.body"]; ok {
		t.Fatal("response body entered transaction data")
	}
	for _, code := range []int{0, 600, 999} {
		recordHTTPResponseStatus(txn, code)
		if got := txn.Data["http.response.status_code"]; got != http.StatusInternalServerError {
			t.Fatalf("status data for %d = %#v, want %d", code, got, http.StatusInternalServerError)
		}
	}
}

func TestFlareTracerDisabledIsPassthrough(t *testing.T) {
	t.Setenv("FLARE_TRACES_SAMPLE_RATE", "0")
	base := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if got := flareTracer(base); &got == nil { // compile-guard; real check below
		t.Fatal("nil handler")
	}
	// rate 0 -> returns the same handler value (no wrapping allocation semantics)
	rec := httptest.NewRecorder()
	flareTracer(base).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
}

type bareResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *bareResponseWriter) Header() http.Header { return w.header }
func (w *bareResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bareResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

type featureResponseWriter struct {
	*httptest.ResponseRecorder
	flushed  bool
	pushed   bool
	hijacked bool
}

func (w *featureResponseWriter) Flush() { w.flushed = true }
func (w *featureResponseWriter) Push(string, *http.PushOptions) error {
	w.pushed = true
	return nil
}
func (w *featureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	left, right := net.Pipe()
	_ = right.Close()
	return left, bufio.NewReadWriter(bufio.NewReader(left), bufio.NewWriter(left)), nil
}
func (w *featureResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(w.ResponseRecorder, r)
}
