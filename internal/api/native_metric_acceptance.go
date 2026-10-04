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

const nativeMetricKeyRetention = 7 * 24 * time.Hour

var errNativeMetricKeyConflict = errors.New("native metric idempotency key has a different payload")

type nativeMetricResult struct {
	Accepted  int64  `json:"accepted"`
	Rejected  int64  `json:"rejected"`
	Dropped   int64  `json:"dropped"`
	ReceiptID string `json:"receipt_id"`
	Invalid   int64  `json:"-"`
	Outcome   string `json:"-"`
}

// A missing key keeps older native senders working. A present key must be one
// opaque token; neither its value nor the body is stored. Clients must reuse
// both the token and exact serialized body bytes on an ambiguous retry.
func nativeMetricKeyHash(r *http.Request) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || len(values[0]) < minBusinessIdempotencyKey ||
		len(values[0]) > maxBusinessIdempotencyKey || !businessIdempotencyToken.MatchString(values[0]) {
		return "", errors.New("Idempotency-Key must be one opaque 16-128 character token")
	}
	sum := sha256.Sum256([]byte(values[0]))
	return hex.EncodeToString(sum[:]), nil
}

func metricPayloadHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// acceptNativeMetricBatch is the 200 boundary. Metric COPY, terminal receipt,
// and (when keyed) replay ledger enter one PostgreSQL transaction. A duplicate
// key waits on the unique index, then returns the winning committed receipt.
// A failed or ambiguous commit returns 503; keyed senders can safely retry.
func (s *Server) acceptNativeMetricBatch(parent context.Context, receipt *ingestReceipt, keyHash, payloadHash string, params []generated.InsertMetricsParams, budget *ingestBudget) (nativeMetricResult, bool, error) {
	var result nativeMetricResult
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
		// Expiry is a hard seven-day retry window. Delete only this project's
		// expired identity before reserving it; the periodic sweep removes
		// expired identities whose sender never reuses the key.
		if _, err := tx.Exec(acceptCtx, `DELETE FROM native_metric_acceptances
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
		err := tx.QueryRow(acceptCtx, `INSERT INTO native_metric_acceptances
			(org_id, project_id, key_hash, payload_hash, receipt_id, expires_at)
			VALUES ($1,$2,$3,$4,$5,now()+($6::bigint * interval '1 microsecond'))
			ON CONFLICT (org_id,project_id,key_hash) DO NOTHING RETURNING receipt_id`,
			receipt.orgID, receipt.projectID, keyHash, payloadHash, receipt.id, nativeMetricKeyRetention.Microseconds()).Scan(&reservedID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Discard the provisional receipt before looking up the winner.
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
				FROM native_metric_acceptances a JOIN ingest_receipts r
				  ON r.id=a.receipt_id AND r.org_id=a.org_id AND r.project_id=a.project_id
				WHERE a.org_id=$1 AND a.project_id=$2 AND a.key_hash=$3 AND a.expires_at>now()
				  AND r.completed_at IS NOT NULL`, receipt.orgID, receipt.projectID, keyHash).
				Scan(&existingHash, &result.ReceiptID, &result.Accepted, &result.Invalid, &result.Dropped, &result.Outcome)
			if err != nil {
				return nativeMetricResult{}, false, err
			}
			if existingHash != payloadHash {
				return nativeMetricResult{}, false, errNativeMetricKeyConflict
			}
			result.Rejected = result.Invalid + result.Dropped
			return result, true, nil
		}
		if err != nil {
			return result, false, err
		}
	}
	result.Accepted = int64(len(params))
	result.Invalid = int64(budget.invalid)
	result.Dropped = int64(budget.dropped)
	result.Rejected = result.Invalid + result.Dropped
	result.ReceiptID = receipt.id
	result.Outcome = classifyIngestRequest(ingestOutcome{
		accepted: uint64(result.Accepted), invalid: uint64(result.Invalid), dropped: uint64(result.Dropped),
	})
	if len(params) > 0 {
		written, err := qtx.InsertMetrics(acceptCtx, params)
		if err != nil {
			return nativeMetricResult{}, false, err
		}
		if written != result.Accepted {
			return nativeMetricResult{}, false, errors.New("metric COPY count did not match prepared rows")
		}
	}
	if _, err := qtx.CompleteIngestReceipt(acceptCtx, generated.CompleteIngestReceiptParams{
		ID: receipt.id, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		ReceivedCount: receipt.received, AcceptedCount: result.Accepted,
		RejectedCount: result.Invalid, DroppedCount: result.Dropped,
		PersistedCount: result.Accepted, Outcome: result.Outcome,
		HttpStatus: http.StatusOK, Retryable: false,
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return nativeMetricResult{}, false, err
	}
	if err := tx.Commit(acceptCtx); err != nil {
		return nativeMetricResult{}, false, err
	}
	return result, false, nil
}

// RunNativeMetricAcceptancePrune bounds retry-key storage. All replicas may
// sweep: row locks split expired batches without touching current identities.
func (s *Server) RunNativeMetricAcceptancePrune(ctx context.Context) {
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
			pruned, err := s.pruneNativeMetricAcceptances(pruneCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("prune native metric retry keys", "error", err)
			} else if pruned == 10000 {
				// A full batch can mean expired keys are arriving faster than
				// the bounded sweep. The warning contains no tenant or key data.
				slog.Warn("native metric retry key prune reached batch cap")
			}
		}
	}
}

func (s *Server) pruneNativeMetricAcceptances(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT org_id,project_id,key_hash FROM native_metric_acceptances
		WHERE expires_at <= now() ORDER BY expires_at LIMIT 10000 FOR UPDATE SKIP LOCKED
	)
	DELETE FROM native_metric_acceptances a USING expired e
	WHERE a.org_id=e.org_id AND a.project_id=e.project_id AND a.key_hash=e.key_hash`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
