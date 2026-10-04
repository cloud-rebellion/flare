package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

// commitOTLPPayload is the OTLP 200 boundary for logs, spans, and metrics.
// The receipt was started before parsing, preserving the existing rejection
// audit. CopyFrom and the receipt's terminal update now commit as one unit.
// A failed transaction leaves its earlier receipt in_progress and retryable;
// an ambiguous commit is never overwritten by a deferred failed update.
func (s *Server) commitOTLPPayload(parent context.Context, receipt *ingestReceipt, budget *ingestBudget, persist func(context.Context, *generated.Queries) error) error {
	if s.pool == nil || receipt == nil {
		return errors.New("OTLP acceptance store unavailable")
	}
	acceptCtx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(acceptCtx)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer done()
		_ = tx.Rollback(cleanupCtx)
	}()
	qtx := generated.New(tx)
	if err := persist(acceptCtx, qtx); err != nil {
		return err
	}
	accepted := receipt.received - int64(budget.rejected())
	if accepted < 0 {
		return errors.New("OTLP accepted count is negative")
	}
	result := classifyIngestRequest(ingestOutcome{
		accepted: uint64(accepted), invalid: uint64(budget.invalid), dropped: uint64(budget.dropped),
	})
	if _, err := qtx.CompleteIngestReceipt(acceptCtx, generated.CompleteIngestReceiptParams{
		ID: receipt.id, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		ReceivedCount: receipt.received, AcceptedCount: accepted,
		RejectedCount: int64(budget.invalid), DroppedCount: int64(budget.dropped),
		PersistedCount: accepted, Outcome: result,
		HttpStatus: http.StatusOK, Retryable: false,
		CompletedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		recordIngestReceiptFailure("complete", receipt)
		return err
	}
	return tx.Commit(acceptCtx)
}
