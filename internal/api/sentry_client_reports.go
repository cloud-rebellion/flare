package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/ingest"
)

func persistSentryClientReport(ctx context.Context, q *generated.Queries, project *generated.Project, payloadHash string, report ingest.ClientReport) error {
	rows := make([]generated.InsertSentryClientReportDiscardsParams, 0, len(report.Discards))
	for _, discard := range report.Discards {
		rows = append(rows, generated.InsertSentryClientReportDiscardsParams{
			OrgID: project.OrgID, ProjectID: project.ID, PayloadSha256: payloadHash,
			ReportedAt: pgtype.Timestamptz{Time: report.ReportedAt, Valid: true},
			Reason:     discard.Reason, Category: discard.Category, Quantity: discard.Quantity,
		})
	}
	n, err := q.InsertSentryClientReportDiscards(ctx, rows)
	if err != nil {
		return err
	}
	if n != int64(len(rows)) {
		return fmt.Errorf("client report store: persisted %d of %d counters", n, len(rows))
	}
	return nil
}

type sdkDiscardBreakdown struct {
	Reason        string     `json:"reason"`
	Category      string     `json:"category"`
	Kind          string     `json:"kind"`
	Quantity      string     `json:"quantity"`
	Reports       int64      `json:"reports"`
	FirstReceived *time.Time `json:"first_received_at"`
	LastReceived  *time.Time `json:"last_received_at"`
}

// handleListSDKDiscards reports the sender's discard counters, not verified
// delivery failures. Category units differ (log_byte versus error, for example)
// and must never be added into a single alleged lost-event total. Quantities
// are decimal strings to preserve large PostgreSQL sums and JavaScript accuracy.
func (s *Server) handleListSDKDiscards(w http.ResponseWriter, r *http.Request) {
	orgID, projectID := orgIDFrom(r.Context()), chi.URLParam(r, "id")
	if orgID == "" {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 168 {
			writeErr(w, http.StatusBadRequest, "hours must be an integer from 1 to 168")
			return
		}
		hours = n
	}
	_, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: projectID, OrgScope: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		slogError(w, "get SDK discard project", err)
		return
	}
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.q.ListSentryClientReportDiscards(r.Context(), generated.ListSentryClientReportDiscardsParams{
		OrgID: orgID, ProjectID: projectID, ReceivedAt: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		slogError(w, "list SDK discards", err)
		return
	}
	breakdown := make([]sdkDiscardBreakdown, 0, len(rows))
	for _, row := range rows {
		breakdown = append(breakdown, sdkDiscardBreakdown{
			Reason: row.Reason, Category: row.Category, Kind: sdkDiscardKind(row.Reason),
			Quantity: row.Quantity, Reports: row.Reports,
			FirstReceived: receiptTimestamp(row.FirstReceivedAt), LastReceived: receiptTimestamp(row.LastReceivedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source_kind": "client_reported", "protocol": "sentry", "window_hours": hours,
		"window_basis": "received_at", "since": since, "breakdown": breakdown,
		"interpretation": "SDK assertions; sampling and filtering may be intentional. Quantities use each category's units and are not server-observed failures. Exact report payload replays are counted once per project; identical reports from independent clients can collide.",
	})
}

func sdkDiscardKind(reason string) string {
	switch reason {
	case "sample_rate", "span_sample_rate":
		return "sampling"
	case "before_send", "event_processor", "ignored":
		return "filtering"
	case "queue_overflow", "buffer_overflow", "cache_overflow":
		return "buffer"
	case "ratelimit_backoff", "backpressure":
		return "rate_limited"
	case "network_error", "send_error":
		return "delivery"
	case "internal_sdk_error":
		return "sdk_error"
	default:
		return "unknown"
	}
}
