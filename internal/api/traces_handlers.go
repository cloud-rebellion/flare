package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/ingest"
)

// handleOTLPTraces ingests OTLP/HTTP traces (protobuf or OTLP/JSON).
func (s *Server) handleOTLPTraces(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	var receipt *ingestReceipt
	receiptStatus := http.StatusOK
	requestOutcome := "rejected"
	defer func() {
		outcome.record("span")
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
	keyHash, err := nativeIngestKeyHash(r)
	if err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if keyHash == "" {
		receipt = s.requireIngestReceipt(r.Context(), w, r, project, "spans", "otlp", &outcome, &requestOutcome, &receiptStatus)
		if receipt == nil {
			return
		}
	}
	asJSON := strings.Contains(r.Header.Get("Content-Type"), "json")
	spans, err := ingest.ParseOTLPTraces(body, asJSON)
	if err != nil {
		if receipt == nil {
			receipt = s.requireIngestReceipt(r.Context(), w, r, project, "spans", "otlp", &outcome, &requestOutcome, &receiptStatus)
			if receipt == nil {
				return
			}
		}
		outcome.invalid = 1
		receiptStatus = http.StatusBadRequest
		s.finishIngestReceipt(r.Context(), receipt, outcome, "rejected", receiptStatus)
		writeErr(w, http.StatusBadRequest, "invalid OTLP traces payload")
		return
	}
	if receipt == nil {
		receipt = newIngestReceipt(w, r, project, "spans", "otlp")
	}
	receipt.setReceived(len(spans))
	requestOutcome = ""
	budget := newIngestBudget()
	stored := 0
	result, err := s.commitOTLPPayload(r.Context(), receipt, budget, keyHash, keyedOTLPPayloadHash(keyHash, body, asJSON), func(ctx context.Context, q *generated.Queries) error {
		var persistErr error
		stored, persistErr = s.persistSpans(ctx, q, project, spans, budget)
		return persistErr
	})
	if err != nil {
		if err == errOTLPKeyConflict {
			outcome.invalid = 1
			requestOutcome = "rejected"
			writeErr(w, http.StatusConflict, "Idempotency-Key was already used for a different OTLP signal or payload")
			return
		}
		outcome.invalid = uint64(budget.invalid)
		outcome.dropped = uint64(budget.dropped)
		// The persistence operation is all-or-nothing for this batch. Rows
		// admitted by the budget were not durably accepted when it failed.
		outcome.failed = uint64(len(spans)) - outcome.invalid - outcome.dropped
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		slog.Error("otlp traces acceptance transaction failed", "project_id", project.ID)
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	w.Header().Set("X-Flare-Receipt-ID", result.receiptID)
	if result.replayed {
		w.Header().Set("Idempotency-Replayed", "true")
		outcome.deduplicated = uint64(result.accepted)
		requestOutcome = result.outcome
		writeJSON(w, http.StatusOK, otlpResponse(int(result.rejected), otlpRejectedSpans))
		return
	}
	// Report rows that were actually admitted to the bounded request budget.
	// The database copy is all-or-nothing, so a successful non-zero count is
	// the durable acceptance count and a zero count is a deliberate drop.
	outcome.accepted = uint64(stored)
	outcome.invalid = uint64(budget.invalid)
	outcome.dropped = uint64(budget.dropped)
	budget.report(project.ID, "spans")
	writeJSON(w, http.StatusOK, otlpResponse(budget.rejected(), otlpRejectedSpans))
}

func (s *Server) persistSpans(ctx context.Context, q *generated.Queries, project *generated.Project, spans []ingest.SpanRecord, budget *ingestBudget) (int, error) {
	spans = spans[:budget.take(len(spans))]
	return persistSpansWithQueries(ctx, q, project, spans)
}

// persistSpansWithQueries lets Sentry envelopes use the same sanitization and
// transactional CopyFrom after its caller has spent the request's row budget.
func persistSpansWithQueries(ctx context.Context, q *generated.Queries, project *generated.Project, spans []ingest.SpanRecord) (int, error) {
	if len(spans) == 0 {
		return 0, nil
	}
	params := make([]generated.InsertSpansParams, 0, len(spans))
	for _, sp := range spans {
		params = append(params, generated.InsertSpansParams{
			// CopyFrom is all-or-nothing: sanitize every client-supplied string
			// so one bad byte cannot fail the whole batch forever under retry.
			TraceID:      ingest.SanitizeColumn(sp.TraceID),
			SpanID:       ingest.SanitizeColumn(sp.SpanID),
			ParentSpanID: ingest.SanitizeColumn(sp.ParentSpanID),
			ProjectID:    project.ID,
			OrgID:        project.OrgID,
			Name:         ingest.SanitizeColumn(sp.Name),
			Kind:         ingest.SanitizeColumn(sp.Kind),
			Status:       ingest.SanitizeColumn(sp.Status),
			StartTime:    pgtype.Timestamptz{Time: sp.Start, Valid: true},
			EndTime:      pgtype.Timestamptz{Time: sp.End, Valid: true},
			DurationMs:   sp.DurationMs,
			Attributes:   ingest.SanitizeJSON(sp.Attributes),
		})
	}
	_, err := q.InsertSpans(ctx, params)
	if err != nil {
		return 0, err
	}
	return len(spans), nil
}

type traceSummary struct {
	TraceID    string    `json:"trace_id"`
	RootName   string    `json:"root_name"`
	SpanCount  int64     `json:"span_count"`
	HasError   bool      `json:"has_error"`
	DurationMs float64   `json:"duration_ms"`
	Started    time.Time `json:"started"`
}

func (s *Server) handleListTraces(w http.ResponseWriter, r *http.Request) {
	traces, err := s.store.ListTraces(r.Context(), chi.URLParam(r, "id"), orgIDFrom(r.Context()),
		time.Now().Add(-maxTraceWindowHours*time.Hour), 100)
	if err != nil {
		slogError(w, "list traces", err)
		return
	}
	out := make([]traceSummary, 0, len(traces))
	for _, t := range traces {
		out = append(out, traceSummary{
			TraceID: t.TraceID, RootName: t.RootName, SpanCount: t.SpanCount,
			HasError: t.HasError, DurationMs: t.DurationMs, Started: t.Started,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type spanResponse struct {
	SpanID       string          `json:"span_id"`
	ParentSpanID string          `json:"parent_span_id"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Status       string          `json:"status"`
	StartUnixMs  int64           `json:"start_unix_ms"`
	DurationMs   float64         `json:"duration_ms"`
	Attributes   json.RawMessage `json:"attributes"`
}

// Read caps, declared once and shared by the REST and MCP surfaces.
//
// They used to be per-surface literals and they had drifted: REST list_issues
// 100 against MCP 200, REST metric points 1000 against MCP 5000, a log window
// of 2160h against an analytics view over the same table at 720h. get_trace and
// list_projects had no cap at all. A cap that differs by surface is not a cap,
// it is a routing decision an attacker gets to make.
const (
	// maxTraceSpans is generous for a real distributed trace and bounds the
	// pathological one: 100,000 spans under a caller-chosen trace id rendered
	// a 52 MB tool result.
	maxTraceSpans = 2000
	// maxTraceWindowHours bounds ListTraces, which grouped the whole spans
	// table for the project before any LIMIT could apply.
	maxTraceWindowHours = 24 * 30
	// maxMetricNames bounds a GROUP BY over a caller-chosen, unbounded-
	// cardinality column.
	maxMetricNames = 5000
	// maxProjectsListed bounds the last uncapped read tool.
	maxProjectsListed = 500
)

func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	spans, err := s.store.GetTraceSpans(r.Context(), chi.URLParam(r, "traceID"), chi.URLParam(r, "id"), orgIDFrom(r.Context()), maxTraceSpans)
	if err != nil {
		slogError(w, "get trace", err)
		return
	}
	if len(spans) == 0 {
		writeErr(w, http.StatusNotFound, "trace not found")
		return
	}
	out := make([]spanResponse, 0, len(spans))
	for _, sp := range spans {
		out = append(out, spanResponse{
			SpanID: sp.SpanID, ParentSpanID: sp.ParentSpanID, Name: sp.Name,
			Kind: sp.Kind, Status: sp.Status,
			StartUnixMs: sp.StartUnixMs, DurationMs: sp.DurationMs,
			Attributes: sp.Attributes,
		})
	}
	// Say so when the trace was cut, rather than presenting a partial trace as
	// a whole one.
	if len(spans) == maxTraceSpans {
		writeJSON(w, http.StatusOK, map[string]any{"spans": out, "truncated": true, "limit": maxTraceSpans})
		return
	}
	writeJSON(w, http.StatusOK, out)
}
