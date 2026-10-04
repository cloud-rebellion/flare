package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

const otlpKeyRetention = 7 * 24 * time.Hour

var errOTLPKeyConflict = errors.New("OTLP idempotency key has a different signal or payload")

type otlpCommitResult struct {
	accepted  int64
	rejected  int64
	dropped   int64
	receiptID string
	outcome   string
	replayed  bool
}

// A retry must use the same decoded bytes and wire encoding. The pillar is
// checked separately, so one project cannot reuse a key across OTLP signals.
func otlpPayloadHash(body []byte, asJSON bool) string {
	h := sha256.New()
	if asJSON {
		_, _ = h.Write([]byte("json\x00"))
	} else {
		_, _ = h.Write([]byte("protobuf\x00"))
	}
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func keyedOTLPPayloadHash(keyHash string, body []byte, asJSON bool) string {
	if keyHash == "" {
		return ""
	}
	return otlpPayloadHash(body, asJSON)
}

// commitOTLPPayload is the OTLP 200 boundary for logs, spans, and metrics.
// Keyless callers keep their pre-started receipt and existing wire behavior.
// Keyed callers start the receipt inside this transaction, so a replay or
// conflict rolls back the provisional row. COPY, terminal receipt, and the
// optional project-scoped retry identity commit together. On an ambiguous
// commit, a keyed sender can safely retry the exact batch; a keyless sender
// may duplicate it, as OTLP normally permits.
func (s *Server) commitOTLPPayload(parent context.Context, receipt *ingestReceipt, budget *ingestBudget, keyHash, payloadHash string, persist func(context.Context, *generated.Queries) error) (otlpCommitResult, error) {
	var result otlpCommitResult
	if s.pool == nil || receipt == nil {
		return result, errors.New("OTLP acceptance store unavailable")
	}
	acceptCtx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(acceptCtx)
	if err != nil {
		return result, err
	}
	defer func() {
		cleanupCtx, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer done()
		_ = tx.Rollback(cleanupCtx)
	}()
	qtx := generated.New(tx)
	if keyHash != "" {
		// A seven-day key can be reused after expiry even if the background
		// sweep has not reached this project's old identity yet.
		if _, err := tx.Exec(acceptCtx, `DELETE FROM otlp_acceptances
			WHERE org_id=$1 AND project_id=$2 AND key_hash=$3 AND expires_at <= now()`,
			receipt.orgID, receipt.projectID, keyHash); err != nil {
			return result, err
		}
		if _, err := qtx.InsertIngestReceipt(acceptCtx, generated.InsertIngestReceiptParams{
			ID: receipt.id, RequestID: receipt.requestID, OrgID: receipt.orgID, ProjectID: receipt.projectID,
			Pillar: receipt.pillar, Protocol: receipt.protocol, Outcome: "in_progress",
			HttpStatus: http.StatusAccepted, Retryable: true,
			ReceivedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		}); err != nil {
			recordIngestReceiptFailure("insert", receipt)
			return result, err
		}
		var reservedID string
		err := tx.QueryRow(acceptCtx, `INSERT INTO otlp_acceptances
			(org_id,project_id,key_hash,payload_hash,pillar,receipt_id,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,now()+($7::bigint * interval '1 microsecond'))
			ON CONFLICT (org_id,project_id,key_hash) DO NOTHING RETURNING receipt_id`,
			receipt.orgID, receipt.projectID, keyHash, payloadHash, receipt.pillar, receipt.id,
			otlpKeyRetention.Microseconds()).Scan(&reservedID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The unique index waits for a concurrent winner to commit. The
			// next READ COMMITTED statement sees its terminal receipt. Returning
			// rolls back this request's provisional receipt before the 200/409.
			var previousPillar, previousHash string
			err = tx.QueryRow(acceptCtx, `SELECT a.pillar,a.payload_hash,r.id,r.accepted_count,
				r.rejected_count,r.dropped_count,r.outcome
				FROM otlp_acceptances a JOIN ingest_receipts r
				  ON r.id=a.receipt_id AND r.org_id=a.org_id AND r.project_id=a.project_id
				WHERE a.org_id=$1 AND a.project_id=$2 AND a.key_hash=$3
				  AND a.expires_at>now() AND r.completed_at IS NOT NULL AND r.protocol='otlp'`,
				receipt.orgID, receipt.projectID, keyHash).
				Scan(&previousPillar, &previousHash, &result.receiptID, &result.accepted,
					&result.rejected, &result.dropped, &result.outcome)
			if err != nil {
				return otlpCommitResult{}, err
			}
			if previousPillar != receipt.pillar || previousHash != payloadHash {
				return otlpCommitResult{}, errOTLPKeyConflict
			}
			result.rejected += result.dropped
			result.replayed = true
			return result, nil
		}
		if err != nil {
			return result, err
		}
	}
	if err := persist(acceptCtx, qtx); err != nil {
		return result, err
	}
	result.accepted = receipt.received - int64(budget.rejected())
	if result.accepted < 0 {
		return otlpCommitResult{}, errors.New("OTLP accepted count is negative")
	}
	result.rejected = int64(budget.rejected())
	result.dropped = int64(budget.dropped)
	result.receiptID = receipt.id
	result.outcome = classifyIngestRequest(ingestOutcome{
		accepted: uint64(result.accepted), invalid: uint64(budget.invalid), dropped: uint64(budget.dropped),
	})
	if _, err := qtx.CompleteIngestReceipt(acceptCtx, generated.CompleteIngestReceiptParams{
		ID: receipt.id, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		ReceivedCount: receipt.received, AcceptedCount: result.accepted,
		RejectedCount: int64(budget.invalid), DroppedCount: int64(budget.dropped),
		PersistedCount: result.accepted, Outcome: result.outcome,
		HttpStatus: http.StatusOK, Retryable: false,
		CompletedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		recordIngestReceiptFailure("complete", receipt)
		return otlpCommitResult{}, err
	}
	if err := tx.Commit(acceptCtx); err != nil {
		return otlpCommitResult{}, err
	}
	return result, nil
}

// Every replica may sweep a bounded set of expired OTLP retry keys. The
// corresponding ingest receipts and telemetry rows remain untouched.
func (s *Server) RunOTLPAcceptancePrune(ctx context.Context) {
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
			pruned, err := s.pruneOTLPAcceptances(pruneCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("prune OTLP retry keys", "error", err)
			} else if pruned == 10000 {
				slog.Warn("OTLP retry key prune reached batch cap")
			}
		}
	}
}

func (s *Server) pruneOTLPAcceptances(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT org_id,project_id,key_hash FROM otlp_acceptances
		WHERE expires_at <= now() ORDER BY expires_at LIMIT 10000 FOR UPDATE SKIP LOCKED
	)
	DELETE FROM otlp_acceptances a USING expired e
	WHERE a.org_id=e.org_id AND a.project_id=e.project_id AND a.key_hash=e.key_hash`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
