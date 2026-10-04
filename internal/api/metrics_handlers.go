package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ingest"
)

// maxMetricLabelBytes bounds one client-supplied label document before it is
// copied into JSONB. Oversized points are rejected as invalid rather than
// silently truncating standard OpenTelemetry attributes; OTLP callers receive
// partial_success and native callers see a lower accepted count. The bound is
// per point, so a normal batch can still contain many independent dimensions.
const maxMetricLabelBytes = 4 << 10

// handleOTLPMetrics ingests OTLP/HTTP metrics (protobuf or OTLP/JSON).
func (s *Server) handleOTLPMetrics(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	var receipt *ingestReceipt
	receiptStatus := http.StatusOK
	requestOutcome := "rejected"
	defer func() {
		outcome.record("metric")
		recordIngestRequest("otlp", outcome, requestOutcome)
	}()
	project, ok := s.authIngest(w, r)
	if !ok {
		return
	}
	body, ok := readIngestBody(w, r)
	if !ok {
		return
	}
	receipt = s.requireIngestReceipt(r.Context(), w, r, project, "metrics", "otlp", &outcome, &requestOutcome, &receiptStatus)
	if receipt == nil {
		return
	}
	asJSON := strings.Contains(r.Header.Get("Content-Type"), "json")
	records, err := ingest.ParseOTLPMetrics(body, asJSON)
	if err != nil {
		outcome.invalid = 1
		receiptStatus = http.StatusBadRequest
		s.finishIngestReceipt(r.Context(), receipt, outcome, "rejected", receiptStatus)
		writeErr(w, http.StatusBadRequest, "invalid OTLP metrics payload")
		return
	}
	receipt.setReceived(len(records))
	requestOutcome = ""
	budget := newIngestBudget()
	if err := s.commitOTLPPayload(r.Context(), receipt, budget, func(ctx context.Context, q *generated.Queries) error {
		return s.persistMetrics(ctx, q, project, records, budget)
	}); err != nil {
		outcome.invalid = uint64(budget.invalid)
		outcome.dropped = uint64(budget.dropped)
		// The persistence operation is all-or-nothing for this batch. Rows
		// admitted by the budget were not durably accepted when it failed.
		outcome.failed = uint64(len(records)) - outcome.invalid - outcome.dropped
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		slog.Error("otlp metrics acceptance transaction failed", "project_id", project.ID)
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	outcome.accepted = uint64(len(records) - budget.rejected())
	outcome.invalid = uint64(budget.invalid)
	outcome.dropped = uint64(budget.dropped)
	budget.report(project.ID, "metrics")
	writeJSON(w, http.StatusOK, otlpResponse(budget.rejected(), otlpRejectedDataPoints))
}

// handleNativeMetrics ingests a bare JSON array of metric points (or {"metrics":[...]}).
func (s *Server) handleNativeMetrics(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	requestOutcome := "rejected"
	defer func() {
		outcome.record("metric")
		recordIngestRequest("native", outcome, requestOutcome)
	}()
	project, ok := s.authIngest(w, r)
	if !ok {
		return
	}
	body, ok := readIngestBody(w, r)
	if !ok {
		return
	}
	keyHash, err := nativeMetricKeyHash(r)
	if err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	records, err := ingest.ParseNativeMetrics(body)
	if err != nil {
		outcome.invalid = 1
		// A malformed request did not accept a signal. The compatibility
		// receipt records its rejection without claiming a successful write.
		if receipt := s.beginIngestReceipt(r.Context(), w, r, project, "metrics", "native"); receipt != nil {
			receipt.setReceived(1)
			s.finishIngestReceipt(r.Context(), receipt, outcome, "rejected", http.StatusBadRequest)
		}
		writeErr(w, http.StatusBadRequest, "invalid metrics payload")
		return
	}
	receipt := newIngestReceipt(w, r, project, "metrics", "native")
	if receipt == nil || s.pool == nil {
		outcome.failed = uint64(len(records))
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	receipt.setReceived(len(records))
	budget := newIngestBudget()
	params := metricInsertParams(project, records[:budget.take(len(records))], budget)
	result, replay, err := s.acceptNativeMetricBatch(r.Context(), receipt, keyHash, metricPayloadHash(body), params, budget)
	if err != nil {
		if err == errNativeMetricKeyConflict {
			outcome.invalid = 1
			writeErr(w, http.StatusConflict, "Idempotency-Key was already used for different metric payload bytes")
			return
		}
		outcome.invalid = uint64(budget.invalid)
		outcome.dropped = uint64(budget.dropped)
		// No prepared row survived a failed transaction. Keep validation and
		// cap rejections distinct from rows that failed to commit.
		outcome.failed = uint64(len(records) - budget.rejected())
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		// PostgreSQL COPY errors may quote a client-supplied metric label or
		// value. Log only a fixed failure class at this ingest boundary.
		slog.Error("native metric acceptance transaction failed", "project_id", project.ID)
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	if replay {
		w.Header().Set("Idempotency-Replayed", "true")
		outcome.deduplicated = uint64(result.Accepted)
	} else {
		outcome.accepted = uint64(result.Accepted)
		outcome.invalid = uint64(result.Invalid)
		outcome.dropped = uint64(result.Dropped)
		budget.report(project.ID, "metrics")
	}
	requestOutcome = result.Outcome
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) persistMetrics(ctx context.Context, q *generated.Queries, project *generated.Project, records []ingest.MetricRecord, budget *ingestBudget) error {
	// Keep the spend at this persistence boundary: the structural row-cap
	// guard scans every persist* method for a direct budget.take call.
	records = records[:budget.take(len(records))]
	params := metricInsertParams(project, records, budget)
	if len(params) == 0 {
		return nil
	}
	_, err := q.InsertMetrics(ctx, params)
	return err
}

func metricInsertParams(project *generated.Project, records []ingest.MetricRecord, budget *ingestBudget) []generated.InsertMetricsParams {
	// Both callers spend the request's row budget before constructing rows.
	records = finiteMetricRows(records, budget)
	params := make([]generated.InsertMetricsParams, 0, len(records))
	for _, m := range records {
		params = append(params, generated.InsertMetricsParams{
			ID:         id.New(),
			ProjectID:  project.ID,
			OrgID:      project.OrgID,
			Name:       ingest.SanitizeColumn(m.Name),
			Kind:       ingest.SanitizeColumn(m.Kind),
			Value:      m.Value,
			Labels:     ingest.SanitizeJSON(m.Labels),
			ObservedAt: pgtype.Timestamptz{Time: m.ObservedAt, Valid: true},
		})
	}
	return params
}

// metricRowsForPersistence applies the request row cap and rejects non-finite
// values or oversized label documents before SQL construction. NaN and +/-Inf
// are valid float64 values but cannot be represented in JSON responses, so
// accepting them would make the sender believe a metric was stored while later
// queries fail to encode it. Labels are rejected as a whole when they exceed
// maxMetricLabelBytes: truncating an OTel attribute document would silently
// change the metric's dimensions.
func finiteMetricRows(records []ingest.MetricRecord, budget *ingestBudget) []ingest.MetricRecord {
	accepted := records[:0]
	for _, m := range records {
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			budget.rejectInvalid()
			continue
		}
		if len(m.Labels) > maxMetricLabelBytes {
			budget.rejectInvalid()
			continue
		}
		accepted = append(accepted, m)
	}
	return accepted
}

type metricNameResponse struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Points   int64     `json:"points"`
	LastSeen time.Time `json:"last_seen"`
}

func (s *Server) handleListMetrics(w http.ResponseWriter, r *http.Request) {
	proj, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	names, err := s.store.ListMetricNames(r.Context(), proj.ID, proj.OrgID, maxMetricNames)
	if err != nil {
		slogError(w, "list metrics", err)
		return
	}
	out := make([]metricNameResponse, 0, len(names))
	for _, m := range names {
		out = append(out, metricNameResponse{Name: m.Name, Kind: m.Kind, Points: m.Points, LastSeen: m.LastSeen})
	}
	writeJSON(w, http.StatusOK, out)
}

type metricPointResponse struct {
	Value      float64         `json:"value"`
	Labels     json.RawMessage `json:"labels"`
	ObservedAt time.Time       `json:"observed_at"`
}

// handleQueryMetric returns the raw points of one metric over a window
// (?name=, ?window= minutes, default 60), oldest first for charting.
func (s *Server) handleQueryMetric(w http.ResponseWriter, r *http.Request) {
	proj, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	windowMin := 60
	if v, err := strconv.Atoi(r.URL.Query().Get("window")); err == nil && v > 0 && v <= 10080 {
		windowMin = v
	}
	since := time.Now().Add(-time.Duration(windowMin) * time.Minute)
	points, err := s.store.QueryMetricSeries(r.Context(), proj.ID, proj.OrgID, name, since, 1000)
	if err != nil {
		slogError(w, "query metric", err)
		return
	}
	// The query returns newest-first; reverse to oldest-first for the chart.
	out := make([]metricPointResponse, 0, len(points))
	for i := len(points) - 1; i >= 0; i-- {
		p := points[i]
		out = append(out, metricPointResponse{Value: p.Value, Labels: p.Labels, ObservedAt: p.ObservedAt})
	}
	writeJSON(w, http.StatusOK, out)
}
