package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

const (
	defaultIngestReceiptLimit = 100
	maxIngestReceiptLimit     = 500
)

// ingestReceiptResponse is deliberately narrower than generated.IngestReceipt:
// it contains request accounting only and never a raw telemetry body. The
// project and organization are already represented by the authenticated route
// and are omitted so this response cannot become a cross-tenant identifier
// oracle if it is copied into another system.
type ingestReceiptResponse struct {
	ID             string     `json:"id"`
	RequestID      string     `json:"request_id,omitempty"`
	Pillar         string     `json:"pillar"`
	Protocol       string     `json:"protocol"`
	ReceivedCount  int64      `json:"received_count"`
	AcceptedCount  int64      `json:"accepted_count"`
	RejectedCount  int64      `json:"rejected_count"`
	DroppedCount   int64      `json:"dropped_count"`
	PersistedCount int64      `json:"persisted_count"`
	Outcome        string     `json:"outcome"`
	HTTPStatus     int16      `json:"http_status"`
	Retryable      bool       `json:"retryable"`
	ReceivedAt     *time.Time `json:"received_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
}

// handleListIngestReceipts returns bounded, newest-first request receipts for
// one project. The query is always scoped by both authenticated org and route
// project so a viewer cannot enumerate another tenant's telemetry accounting.
func (s *Server) handleListIngestReceipts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListIngestReceiptsByProject(r.Context(), generated.ListIngestReceiptsByProjectParams{
		OrgID:     orgIDFrom(r.Context()),
		ProjectID: chi.URLParam(r, "id"),
		Limit:     ingestReceiptLimit(r),
	})
	if err != nil {
		slogError(w, "list ingest receipts", err)
		return
	}
	out := make([]ingestReceiptResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		out = append(out, toIngestReceiptResponse(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func ingestReceiptLimit(r *http.Request) int32 {
	limit := defaultIngestReceiptLimit
	if v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); err == nil && v > 0 {
		if v > maxIngestReceiptLimit {
			v = maxIngestReceiptLimit
		}
		limit = v
	}
	return int32(limit)
}

func toIngestReceiptResponse(row *generated.IngestReceipt) ingestReceiptResponse {
	out := ingestReceiptResponse{
		ID:             row.ID,
		Pillar:         row.Pillar,
		Protocol:       row.Protocol,
		ReceivedCount:  row.ReceivedCount,
		AcceptedCount:  row.AcceptedCount,
		RejectedCount:  row.RejectedCount,
		DroppedCount:   row.DroppedCount,
		PersistedCount: row.PersistedCount,
		Outcome:        row.Outcome,
		HTTPStatus:     row.HttpStatus,
		Retryable:      row.Retryable,
		ReceivedAt:     receiptTimestamp(row.ReceivedAt),
		CompletedAt:    receiptTimestamp(row.CompletedAt),
		CreatedAt:      receiptTimestamp(row.CreatedAt),
	}
	if row.RequestID.Valid {
		out.RequestID = row.RequestID.String
	}
	return out
}

func receiptTimestamp(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}
