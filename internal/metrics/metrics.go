// Package metrics provides the small, process-local Prometheus surface used by
// Flare. It deliberately has no dependencies outside the standard library.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var durationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type key struct{ method, route, status string }
type series struct {
	total uint64
	sum   float64
	bucks [len(durationBuckets)]uint64
}

// Metrics stores bounded HTTP request counters and duration histograms.
type Metrics struct {
	mu     sync.Mutex
	m      map[key]*series
	custom map[string]*customSeries
}

type customSeries struct {
	labels    map[string]string
	counter   uint64
	histogram bool
	count     uint64
	sum       float64
	bucks     [len(durationBuckets)]uint64
}

type customSnapshot struct {
	name      string
	labels    string
	counter   uint64
	histogram bool
	count     uint64
	sum       float64
	bucks     [len(durationBuckets)]uint64
}

func New() *Metrics {
	return &Metrics{m: make(map[key]*series), custom: make(map[string]*customSeries)}
}

// Default is the process-wide metrics registry used by Flare.
var Default = New()

// Inc increments a custom counter in the process-wide registry.
func Inc(name string, labels map[string]string) { Default.Inc(name, labels) }

// Add increments a custom counter by n in the process-wide registry.
//
// Counter deltas let high-volume paths record bounded aggregate outcomes
// without looping once per item. A zero delta is a no-op.
func Add(name string, labels map[string]string, n uint64) { Default.Add(name, labels, n) }

// Observe records a custom duration histogram in seconds in the process-wide registry.
func Observe(name string, labels map[string]string, seconds float64) {
	Default.Observe(name, labels, seconds)
}

// Inc increments a bounded custom counter. Invalid names, excessive labels,
// and oversized label values are ignored to keep this surface cardinality-safe.
func (m *Metrics) Inc(name string, labels map[string]string) {
	m.Add(name, labels, 1)
}

// Add increments a bounded custom counter by n. Invalid names, excessive labels,
// and oversized label values are ignored to keep this surface cardinality-safe.
func (m *Metrics) Add(name string, labels map[string]string, n uint64) {
	if n == 0 {
		return
	}
	m.customObserve(name, labels, 0, false, n)
}

// Observe records a bounded custom histogram in seconds.
func (m *Metrics) Observe(name string, labels map[string]string, seconds float64) {
	m.customObserve(name, labels, seconds, true, 0)
}

func (m *Metrics) customObserve(name string, labels map[string]string, seconds float64, histogram bool, counter uint64) {
	if !validName(name) || len(labels) > 8 {
		return
	}
	clean := make(map[string]string, len(labels))
	for k, v := range labels {
		if !validName(k) || len(v) > 64 {
			return
		}
		clean[k] = v
	}
	// A process-local cap prevents a caller from turning labels into an
	// unbounded memory sink even when values are individually bounded.
	id := name + "\x00" + labelKey(clean)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.custom[id] == nil && len(m.custom) >= 1000 {
		return
	}
	s := m.custom[id]
	if s == nil {
		s = &customSeries{labels: clean}
		m.custom[id] = s
	}
	if counter > 0 {
		s.counter += counter
	}
	if histogram {
		s.histogram = true
		s.count++
		s.sum += seconds
		for i, b := range durationBuckets {
			if seconds <= b {
				s.bucks[i]++
			}
		}
	}
}

// Middleware records requests. route may return a chi route pattern; when it
// is empty, the request path is normalized to avoid IDs becoming labels.
func (m *Metrics) Middleware(route func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rw, r)
			label := ""
			if route != nil {
				label = route(r)
			}
			label = normalizeRoute(label, r.URL.Path)
			m.observe(r.Method, label, rw.status, time.Since(start).Seconds())
		})
	}
}

func (m *Metrics) observe(method, route string, status int, seconds float64) {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
	default:
		method = "OTHER"
	}
	if route == "" {
		route = "/"
	}
	k := key{method: method, route: route, status: strconv.Itoa(status)}
	m.mu.Lock()
	s := m.m[k]
	if s == nil {
		s = &series{}
		m.m[k] = s
	}
	s.total++
	s.sum += seconds
	for i, bound := range durationBuckets {
		if seconds <= bound {
			s.bucks[i]++
		}
	}
	m.mu.Unlock()
}

// Handler renders the Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.mu.Lock()
		keys := make([]key, 0, len(m.m))
		httpValues := make(map[key]series, len(m.m))
		for k := range m.m {
			keys = append(keys, k)
			httpValues[k] = *m.m[k]
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].route != keys[j].route {
				return keys[i].route < keys[j].route
			}
			if keys[i].method != keys[j].method {
				return keys[i].method < keys[j].method
			}
			return keys[i].status < keys[j].status
		})
		custom := make([]customSnapshot, 0, len(m.custom))
		for id := range m.custom {
			s := m.custom[id]
			custom = append(custom, customSnapshot{
				name:      id[:strings.IndexByte(id, 0)],
				labels:    formatLabels(s.labels),
				counter:   s.counter,
				histogram: s.histogram,
				count:     s.count,
				sum:       s.sum,
				bucks:     s.bucks,
			})
		}
		sort.Slice(custom, func(i, j int) bool {
			if custom[i].name != custom[j].name {
				return custom[i].name < custom[j].name
			}
			return custom[i].labels < custom[j].labels
		})
		m.mu.Unlock()

		fmt.Fprintln(w, "# HELP flare_http_requests_total HTTP requests handled by Flare.")
		fmt.Fprintln(w, "# TYPE flare_http_requests_total counter")
		fmt.Fprintln(w, "# HELP flare_http_request_duration_seconds HTTP request duration in seconds.")
		fmt.Fprintln(w, "# TYPE flare_http_request_duration_seconds histogram")
		fmt.Fprintln(w, "# HELP flare_integration_requests_total Outbound integration attempts classified by provider, operation, channel, and outcome.")
		fmt.Fprintln(w, "# TYPE flare_integration_requests_total counter")
		fmt.Fprintln(w, "# HELP flare_integration_request_duration_seconds Outbound integration request duration in seconds.")
		fmt.Fprintln(w, "# TYPE flare_integration_request_duration_seconds histogram")
		fmt.Fprintln(w, "# HELP flare_ingest_requests_total Ingest requests classified by transport and acceptance outcome.")
		fmt.Fprintln(w, "# TYPE flare_ingest_requests_total counter")
		fmt.Fprintln(w, "# HELP flare_ingest_items_total Ingest items classified by signal and persistence outcome.")
		fmt.Fprintln(w, "# TYPE flare_ingest_items_total counter")
		fmt.Fprintln(w, "# HELP flare_ingest_receipts_total Durable ingest receipt writes classified by operation and outcome.")
		fmt.Fprintln(w, "# TYPE flare_ingest_receipts_total counter")
		fmt.Fprintln(w, "# HELP flare_ai_action_receipts_total Durable AI action receipt writes classified by operation and outcome.")
		fmt.Fprintln(w, "# TYPE flare_ai_action_receipts_total counter")
		fmt.Fprintln(w, "# HELP flare_connector_receipts_total Durable connector receipt writes classified by operation and outcome.")
		fmt.Fprintln(w, "# TYPE flare_connector_receipts_total counter")
		fmt.Fprintln(w, "# HELP flare_usage_bucket_writes_total Durable tenant usage bucket writes classified by fixed surface, operation, and outcome.")
		fmt.Fprintln(w, "# TYPE flare_usage_bucket_writes_total counter")
		for _, k := range keys {
			s := httpValues[k]
			labels := fmt.Sprintf("method=%q,route=%q,status=%q", k.method, k.route, k.status)
			fmt.Fprintf(w, "flare_http_requests_total{%s} %d\n", labels, s.total)
			for i, bound := range durationBuckets {
				fmt.Fprintf(w, "flare_http_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(bound, 'f', -1, 64), s.bucks[i])
			}
			fmt.Fprintf(w, "flare_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, s.total)
			fmt.Fprintf(w, "flare_http_request_duration_seconds_sum{%s} %s\n", labels, strconv.FormatFloat(s.sum, 'f', 6, 64))
			fmt.Fprintf(w, "flare_http_request_duration_seconds_count{%s} %d\n", labels, s.total)
		}
		for _, s := range custom {
			labels := s.labels
			name := s.name
			if s.counter > 0 {
				fmt.Fprintf(w, "%s_total{%s} %d\n", name, labels, s.counter)
			}
			if s.histogram {
				for i, b := range durationBuckets {
					fmt.Fprintf(w, "%s_duration_seconds_bucket{%s,le=%q} %d\n", name, labels, strconv.FormatFloat(b, 'f', -1, 64), s.bucks[i])
				}
				fmt.Fprintf(w, "%s_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", name, labels, s.count)
				fmt.Fprintf(w, "%s_duration_seconds_sum{%s} %s\n", name, labels, strconv.FormatFloat(s.sum, 'f', 6, 64))
				fmt.Fprintf(w, "%s_duration_seconds_count{%s} %d\n", name, labels, s.count)
			}
		}
	})
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(',')
	}
	return b.String()
}
func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]string, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return strings.Join(vals, ",")
}

type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}
func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(p)
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) Flush() {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("response writer does not support hijacking")
}
func (w *responseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
func (w *responseWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func normalizeRoute(pattern, path string) string {
	if pattern != "" && strings.HasPrefix(pattern, "/") {
		return pattern
	}
	// Unknown paths may include arbitrary user-controlled segments. Collapse
	// them to one label rather than leaking slugs or creating unbounded series.
	return "<unmatched>"
}
