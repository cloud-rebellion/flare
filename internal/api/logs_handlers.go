package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/ai"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ingest"
	"github.com/bright-interaction/flare/internal/telemetry"
)

// handleOTLPLogs ingests OTLP/HTTP logs (protobuf or OTLP/JSON). Auth is the
// DSN public key via X-Flare-Key (OTEL_EXPORTER_OTLP_HEADERS) or sentry_key.
func (s *Server) handleOTLPLogs(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	var receipt *ingestReceipt
	receiptStatus := http.StatusOK
	requestOutcome := "rejected"
	defer func() {
		outcome.record("log")
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
	receipt = s.requireIngestReceipt(r.Context(), w, r, project, "logs", "otlp", &outcome, &requestOutcome, &receiptStatus)
	if receipt == nil {
		return
	}
	asJSON := strings.Contains(r.Header.Get("Content-Type"), "json")
	records, err := ingest.ParseOTLPLogs(body, asJSON)
	if err != nil {
		outcome.invalid = 1
		receiptStatus = http.StatusBadRequest
		s.finishIngestReceipt(r.Context(), receipt, outcome, "rejected", receiptStatus)
		writeErr(w, http.StatusBadRequest, "invalid OTLP logs payload")
		return
	}
	receipt.setReceived(len(records))
	requestOutcome = ""
	budget := newIngestBudget()
	if err := s.commitOTLPPayload(r.Context(), receipt, budget, func(ctx context.Context, q *generated.Queries) error {
		return s.persistLogs(ctx, q, project, records, budget)
	}); err != nil {
		outcome.invalid = uint64(budget.invalid)
		outcome.dropped = uint64(budget.dropped)
		// The persistence operation is all-or-nothing for this batch. Rows
		// admitted by the budget were not durably accepted when it failed.
		outcome.failed = uint64(len(records)) - outcome.invalid - outcome.dropped
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		slog.Error("otlp logs acceptance transaction failed", "project_id", project.ID)
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	outcome.accepted = uint64(len(records) - budget.rejected())
	outcome.invalid = uint64(budget.invalid)
	outcome.dropped = uint64(budget.dropped)
	budget.report(project.ID, "logs")
	// OTLP success response, carrying partial_success when the row cap truncated
	// the export so the collector does not count dropped rows as delivered.
	writeJSON(w, http.StatusOK, otlpResponse(budget.rejected(), otlpRejectedLogRecords))
}

// handleNativeLogs ingests a JSON array of log records (the thin SDK path).
func (s *Server) handleNativeLogs(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	var receipt *ingestReceipt
	receiptStatus := http.StatusOK
	requestOutcome := "rejected"
	defer func() {
		outcome.record("log")
		recordIngestRequest("native", outcome, requestOutcome)
		s.finishIngestReceipt(r.Context(), receipt, outcome, requestOutcome, receiptStatus)
	}()
	project, ok := s.authIngest(w, r)
	if !ok {
		return
	}
	body, ok := readIngestBody(w, r)
	if !ok {
		return
	}
	receipt = s.requireIngestReceipt(r.Context(), w, r, project, "logs", "native", &outcome, &requestOutcome, &receiptStatus)
	if receipt == nil {
		return
	}
	records, err := ingest.ParseNativeLogs(body)
	if err != nil {
		outcome.invalid = 1
		receiptStatus = http.StatusBadRequest
		writeErr(w, http.StatusBadRequest, "invalid logs payload")
		return
	}
	receipt.setReceived(len(records))
	requestOutcome = ""
	budget := newIngestBudget()
	if err := s.persistLogs(r.Context(), s.q, project, records, budget); err != nil {
		outcome.invalid = uint64(budget.invalid)
		outcome.dropped = uint64(budget.dropped)
		// The persistence operation is all-or-nothing for this batch. Rows
		// admitted by the budget were not durably accepted when it failed.
		outcome.failed = uint64(len(records)) - outcome.invalid - outcome.dropped
		requestOutcome = "failed"
		receiptStatus = http.StatusServiceUnavailable
		slogError(w, "persist logs", err)
		return
	}
	outcome.accepted = uint64(len(records) - budget.rejected())
	outcome.invalid = uint64(budget.invalid)
	outcome.dropped = uint64(budget.dropped)
	budget.report(project.ID, "logs")
	// accepted is what was STORED, not what was posted: reporting the posted
	// count after truncating would be the silent loss the cap must not introduce.
	writeJSON(w, http.StatusOK, map[string]int{"accepted": len(records) - budget.rejected()})
}

func (s *Server) persistLogs(ctx context.Context, q *generated.Queries, project *generated.Project, records []ingest.LogRecord, budget *ingestBudget) error {
	// Spend the request's row budget first: an OTLP export nests
	// resource -> scope -> record, so the byte cap alone leaves the row count
	// unbounded. See maxIngestRecords.
	records = records[:budget.take(len(records))]
	if len(records) == 0 {
		return nil
	}
	params := make([]generated.InsertLogsParams, 0, len(records))
	for _, rec := range records {
		params = append(params, generated.InsertLogsParams{
			ID:        id.New(),
			ProjectID: project.ID,
			OrgID:     project.OrgID,
			Severity:  ingest.SanitizeColumn(rec.Severity),
			// InsertLogs is a CopyFrom: it is all-or-nothing, so one record
			// carrying a NUL byte or invalid UTF-8 failed the whole batch, and an
			// OTLP collector's retry made that record a permanent poison pill.
			Body:       ingest.SanitizeColumn(rec.Body),
			Attributes: ingest.SanitizeJSON(rec.Attributes),
			TraceID:    ingest.SanitizeColumn(rec.TraceID),
			SpanID:     ingest.SanitizeColumn(rec.SpanID),
			ObservedAt: pgtype.Timestamptz{Time: rec.ObservedAt, Valid: true},
		})
	}
	_, err := q.InsertLogs(ctx, params)
	return err
}

type logResponse struct {
	ID         string          `json:"id"`
	Severity   string          `json:"severity"`
	Body       string          `json:"body"`
	Attributes json.RawMessage `json:"attributes"`
	TraceID    string          `json:"trace_id"`
	SpanID     string          `json:"span_id"`
	ObservedAt time.Time       `json:"observed_at"`
}

const (
	// maxLogWindowHours clamps `hours`. 90 days is past any retention setting
	// this ships with, so a larger value only asks the database to prove the
	// rows are absent.
	maxLogWindowHours = 24 * 90
	// maxLogLimit caps a page. 100 is the default; 500 is enough for an export
	// loop without letting one request pull an unbounded slice into memory.
	maxLogLimit = 500
)

// handleSearchLogs is the authenticated dashboard query over the hot tier.
func (s *Server) handleSearchLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := telemetry.LogFilter{Limit: 100}
	f.Query = searchTerm(q.Get("q"))
	if v := strings.TrimSpace(q.Get("severity")); v != "" {
		f.Severity = &v
	}
	if v := strings.TrimSpace(q.Get("trace_id")); v != "" {
		f.TraceID = &v
	}
	// Time window. `hours` is what the UI sends; `since` (RFC3339) is the
	// precise form for API and MCP callers. Without either, the query scanned
	// the entire retained history and returned the newest 100, so an operator
	// looking at an incident from Tuesday had no way to ask for Tuesday.
	if v := strings.TrimSpace(q.Get("since")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = &t
		} else {
			writeErr(w, http.StatusBadRequest, "since must be an RFC3339 timestamp")
			return
		}
	} else if v := strings.TrimSpace(q.Get("hours")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "hours must be a positive integer")
			return
		}
		// Clamped: a huge value is the same unbounded scan the window exists to
		// prevent, and nothing is retained beyond it anyway.
		if n > maxLogWindowHours {
			n = maxLogWindowHours
		}
		t := time.Now().Add(-time.Duration(n) * time.Hour)
		f.Since = &t
	}
	// Keyset paging cursor: the previous page's oldest row.
	if v := strings.TrimSpace(q.Get("before")); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before must be an RFC3339 timestamp")
			return
		}
		f.Before = &t
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > maxLogLimit {
			n = maxLogLimit
		}
		f.Limit = int32(n)
	}

	clampLogWindow(&f)

	logs, err := s.store.SearchLogs(r.Context(), chi.URLParam(r, "id"), orgIDFrom(r.Context()), f)
	if err != nil {
		slogError(w, "search logs", err)
		return
	}
	out := toLogResponses(logs)
	// next_before is the cursor for the following page, present only when this
	// page was full. An empty value means "no more", which is what lets the UI
	// distinguish the end of the data from a page that happens to be short.
	resp := map[string]any{"logs": out}
	if int32(len(logs)) == f.Limit && len(logs) > 0 {
		resp["next_before"] = logs[len(logs)-1].ObservedAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// clampLogWindow gives the search a floor, and gives it to every caller.
//
// `hours` was clamped and `since` was not, so ?since=0001-01-01T00:00:00Z with
// ?q=%% was a full unindexed ILIKE scan of the entire retained logs table, on
// demand, from any viewer session or read-only API key. An absent window is the
// same scan, so it gets the floor too: the query already returns the newest
// rows first, and nothing is retained beyond the window anyway.
func clampLogWindow(f *telemetry.LogFilter) {
	floor := time.Now().Add(-maxLogWindowHours * time.Hour)
	if f.Since == nil || f.Since.Before(floor) {
		f.Since = &floor
	}
}

// toLogResponses maps stored logs to the clean API shape. Used by the REST
// dashboard, where a human views their own in-region logs (not an egress
// boundary), so bodies + attributes are returned as-is.
func toLogResponses(logs []telemetry.Log) []logResponse {
	out := make([]logResponse, 0, len(logs))
	for _, l := range logs {
		out = append(out, logResponse{
			ID: l.ID, Severity: l.Severity, Body: l.Body, Attributes: l.Attributes,
			TraceID: l.TraceID, SpanID: l.SpanID, ObservedAt: l.ObservedAt,
		})
	}
	return out
}

// toLogResponsesScrubbed is the MCP-egress variant: it runs each log body +
// attributes through ai.Scrub before the rows leave for an LLM. The logs pillar
// has no per-log sensitive flag (unlike issues), and bodies routinely carry PII
// (emails, tokens, ids), so search_logs must scrub on the way out - matching how
// get_issue scrubs sensitive event messages.
func toLogResponsesScrubbed(logs []telemetry.Log) []logResponse {
	out := toLogResponses(logs)
	for i := range out {
		out[i].Body = ai.Scrub(out[i].Body)
		if len(out[i].Attributes) > 0 {
			out[i].Attributes = json.RawMessage(ai.ScrubJSON(out[i].Attributes))
		}
	}
	return out
}
