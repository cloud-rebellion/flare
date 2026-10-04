package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/ingest"
)

const nativeLogKeyRetention = 7 * 24 * time.Hour

var errNativeLogKeyConflict = errors.New("native log idempotency key has a different payload")

type nativeLogResult struct {
	Accepted  int64  `json:"accepted"`
	Rejected  int64  `json:"rejected"`
	Dropped   int64  `json:"dropped"`
	ReceiptID string `json:"receipt_id"`
	Invalid   int64  `json:"-"`
	Outcome   string `json:"-"`
}

// acceptNativeLogBatch is the native log 200 boundary. Log COPY, terminal
// receipt, and the optional project-scoped retry identity commit together.
// A duplicate key waits for the first transaction and returns its receipt;
// a failed or ambiguous commit returns 503 so the sender can retry that key.
func (s *Server) acceptNativeLogBatch(parent context.Context, receipt *ingestReceipt, project *generated.Project, keyHash, payloadHash string, records []ingest.LogRecord, budget *ingestBudget) (nativeLogResult, bool, error) {
	var result nativeLogResult
	acceptCtx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(acceptCtx)
	if err != nil {
		return result, false, err
	}
	defer func() {
		cleanupCtx, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer done()
		_ = tx.Rollback(cleanupCtx)
	}()
	qtx := generated.New(tx)
	if keyHash != "" {
		// A reused key after expiry starts a new operation even when the
		// periodic sweep has not yet removed its old identity.
		if _, err := tx.Exec(acceptCtx, `DELETE FROM native_log_acceptances
			WHERE org_id=$1 AND project_id=$2 AND key_hash=$3 AND expires_at <= now()`, receipt.orgID, receipt.projectID, keyHash); err != nil {
			return result, false, err
		}
	}
	now := time.Now().UTC()
	if _, err := qtx.InsertIngestReceipt(acceptCtx, generated.InsertIngestReceiptParams{
		ID: receipt.id, RequestID: receipt.requestID, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		Pillar: receipt.pillar, Protocol: receipt.protocol, Outcome: "in_progress",
		HttpStatus: http.StatusAccepted, Retryable: true,
		ReceivedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return result, false, err
	}
	if keyHash != "" {
		var reservedID string
		err := tx.QueryRow(acceptCtx, `INSERT INTO native_log_acceptances
			(org_id, project_id, key_hash, payload_hash, receipt_id, expires_at)
			VALUES ($1,$2,$3,$4,$5,now()+($6::bigint * interval '1 microsecond'))
			ON CONFLICT (org_id,project_id,key_hash) DO NOTHING RETURNING receipt_id`,
			receipt.orgID, receipt.projectID, keyHash, payloadHash, receipt.id, nativeLogKeyRetention.Microseconds()).Scan(&reservedID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The provisional receipt is invisible to the replay lookup.
			rollbackCtx, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			rollbackErr := tx.Rollback(rollbackCtx)
			done()
			if rollbackErr != nil {
				return result, false, rollbackErr
			}
			lookupCtx, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			defer done()
			var existingHash string
			err = s.pool.QueryRow(lookupCtx, `SELECT a.payload_hash,r.id,r.accepted_count,r.rejected_count,r.dropped_count,r.outcome
				FROM native_log_acceptances a JOIN ingest_receipts r
				  ON r.id=a.receipt_id AND r.org_id=a.org_id AND r.project_id=a.project_id
				WHERE a.org_id=$1 AND a.project_id=$2 AND a.key_hash=$3 AND a.expires_at>now()
				  AND r.completed_at IS NOT NULL`, receipt.orgID, receipt.projectID, keyHash).
				Scan(&existingHash, &result.ReceiptID, &result.Accepted, &result.Invalid, &result.Dropped, &result.Outcome)
			if err != nil {
				return nativeLogResult{}, false, err
			}
			if existingHash != payloadHash {
				return nativeLogResult{}, false, errNativeLogKeyConflict
			}
			result.Rejected = result.Invalid + result.Dropped
			return result, true, nil
		}
		if err != nil {
			return result, false, err
		}
	}
	if err := s.persistLogs(acceptCtx, qtx, project, records, budget); err != nil {
		return nativeLogResult{}, false, err
	}
	result.Accepted = int64(len(records) - budget.rejected())
	result.Invalid = int64(budget.invalid)
	result.Dropped = int64(budget.dropped)
	result.Rejected = result.Invalid + result.Dropped
	result.ReceiptID = receipt.id
	result.Outcome = classifyIngestRequest(ingestOutcome{
		accepted: uint64(result.Accepted), invalid: uint64(result.Invalid), dropped: uint64(result.Dropped),
	})
	if _, err := qtx.CompleteIngestReceipt(acceptCtx, generated.CompleteIngestReceiptParams{
		ID: receipt.id, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		ReceivedCount: receipt.received, AcceptedCount: result.Accepted,
		RejectedCount: result.Invalid, DroppedCount: result.Dropped,
		PersistedCount: result.Accepted, Outcome: result.Outcome,
		HttpStatus: http.StatusOK, Retryable: false,
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return nativeLogResult{}, false, err
	}
	if err := tx.Commit(acceptCtx); err != nil {
		return nativeLogResult{}, false, err
	}
	return result, false, nil
}

// Every replica sweeps a bounded number of expired keys. This never deletes
// the retained log rows or their ingest receipts.
func (s *Server) RunNativeLogAcceptancePrune(ctx context.Context) {
	if s.pool == nil {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			pruned, err := s.pruneNativeLogAcceptances(pruneCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("prune native log retry keys", "error", err)
			} else if pruned == 10000 {
				slog.Warn("native log retry key prune reached batch cap")
			}
		}
	}
}

func (s *Server) pruneNativeLogAcceptances(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT org_id,project_id,key_hash FROM native_log_acceptances
		WHERE expires_at <= now() ORDER BY expires_at LIMIT 10000 FOR UPDATE SKIP LOCKED
	)
	DELETE FROM native_log_acceptances a USING expired e
	WHERE a.org_id=e.org_id AND a.project_id=e.project_id AND a.key_hash=e.key_hash`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
