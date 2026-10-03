package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
)

const (
	defaultAIActionReceiptLimit = 100
	maxAIActionReceiptLimit     = 500
)

// aiActionReceiptResponse is deliberately payload-free. It reports the
// tenant-scoped operation outcome while omitting org/project identifiers,
// prompt text, completion text, credentials, and provider error details.
type aiActionReceiptResponse struct {
	ID             string     `json:"id"`
	IssueID        string     `json:"issue_id"`
	Operation      string     `json:"operation"`
	Status         string     `json:"status"`
	Provider       string     `json:"provider"`
	EvidenceDigest string     `json:"evidence_digest,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Retryable      bool       `json:"retryable"`
	ErrorCode      string     `json:"error_code,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
}

// handleListAIActionReceipts returns bounded, newest-first AI action receipts
// for one project. The query is scoped by both the authenticated organization
// and route project so a viewer cannot enumerate another tenant's operations.
func (s *Server) handleListAIActionReceipts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListAIActionReceiptsByProject(r.Context(), generated.ListAIActionReceiptsByProjectParams{
		OrgID:     orgIDFrom(r.Context()),
		ProjectID: chi.URLParam(r, "id"),
		Limit:     aiActionReceiptLimit(r),
	})
	if err != nil {
		slogError(w, "list ai action receipts", err)
		return
	}
	out := make([]aiActionReceiptResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		out = append(out, toAIActionReceiptResponse(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func aiActionReceiptLimit(r *http.Request) int32 {
	limit := defaultAIActionReceiptLimit
	if v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); err == nil && v > 0 {
		if v > maxAIActionReceiptLimit {
			v = maxAIActionReceiptLimit
		}
		limit = v
	}
	return int32(limit)
}

func toAIActionReceiptResponse(row *generated.AiActionReceipt) aiActionReceiptResponse {
	out := aiActionReceiptResponse{
		ID:             row.ID,
		IssueID:        row.IssueID,
		Operation:      row.Operation,
		Status:         row.Status,
		Provider:       row.Provider,
		EvidenceDigest: row.EvidenceDigest,
		StartedAt:      receiptTimestamp(row.StartedAt),
		CompletedAt:    receiptTimestamp(row.CompletedAt),
		Retryable:      row.Retryable,
		CreatedAt:      receiptTimestamp(row.CreatedAt),
	}
	if row.ErrorCode.Valid {
		out.ErrorCode = row.ErrorCode.String
	}
	return out
}
