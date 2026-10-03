package api

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// handleLogVolume returns hourly log counts by severity (columnar, DuckDB over
// the hot+cold union). 503 when analytics is disabled.
func (s *Server) handleLogVolume(w http.ResponseWriter, r *http.Request) {
	if s.analytics == nil {
		writeErr(w, http.StatusServiceUnavailable, "analytics not available")
		return
	}
	buckets, err := s.analytics.LogVolumeByHour(r.Context(), chi.URLParam(r, "id"), orgIDFrom(r.Context()), sinceHours(r))
	if err != nil {
		slogError(w, "analytics log volume", err)
		return
	}
	writeJSON(w, http.StatusOK, buckets)
}

// handleSpanLatency returns p50/p95/p99 span duration per operation.
func (s *Server) handleSpanLatency(w http.ResponseWriter, r *http.Request) {
	if s.analytics == nil {
		writeErr(w, http.StatusServiceUnavailable, "analytics not available")
		return
	}
	rows, err := s.analytics.SpanLatencyPercentiles(r.Context(), chi.URLParam(r, "id"), orgIDFrom(r.Context()), sinceHours(r))
	if err != nil {
		slogError(w, "analytics span latency", err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleServiceLevel returns a bounded availability/error-budget aggregate
// over received spans. It is intentionally explicit that the result is based
// on sampled telemetry; missing traces cannot establish request availability.
func (s *Server) handleServiceLevel(w http.ResponseWriter, r *http.Request) {
	if s.analytics == nil {
		writeErr(w, http.StatusServiceUnavailable, "analytics not available")
		return
	}
	latencyMs := analyticsFloatQuery(r, "latency_ms", 1000)
	if latencyMs < 0 || math.IsNaN(latencyMs) || math.IsInf(latencyMs, 0) || latencyMs > 24*60*60*1000 {
		writeErr(w, http.StatusBadRequest, "latency_ms must be between 0 and 86400000")
		return
	}
	target := analyticsFloatQuery(r, "target", 0.999)
	if target <= 0 || target >= 1 || math.IsNaN(target) || math.IsInf(target, 0) {
		writeErr(w, http.StatusBadRequest, "target must be greater than 0 and less than 1")
		return
	}
	operation := strings.TrimSpace(r.URL.Query().Get("span_name"))
	if len(operation) > 128 || strings.ContainsRune(operation, '\x00') {
		writeErr(w, http.StatusBadRequest, "span_name must be at most 128 bytes")
		return
	}
	row, err := s.analytics.SpanServiceLevel(r.Context(), chi.URLParam(r, "id"), orgIDFrom(r.Context()), sinceHours(r), operation, latencyMs, target)
	if err != nil {
		slogError(w, "analytics service level", err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func analyticsFloatQuery(r *http.Request, key string, fallback float64) float64 {
	if raw := strings.TrimSpace(r.URL.Query().Get(key)); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil {
			return v
		}
	}
	return fallback
}

// sinceHours reads ?hours= (default 24, max 720 = 30 days hot window).
func sinceHours(r *http.Request) int {
	if v, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && v > 0 && v <= 720 {
		return v
	}
	return 24
}
