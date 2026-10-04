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
)

const clientConnectorKeyRetention = 7 * 24 * time.Hour

var errClientConnectorKeyConflict = errors.New("client connector idempotency key has a different payload")

// acceptClientConnectorAttempt is the public DSN endpoint's 202 boundary. The
// attempt, terminal tenant receipt, and optional retry identity commit as one
// unit. The caller supplies only bounded health fields; this function pins
// project scope and client_reported provenance before writing anything.
func (s *Server) acceptClientConnectorAttempt(parent context.Context, receipt *ingestReceipt, keyHash, payloadHash string, params generated.InsertConnectorAttemptParams) (connectorAttemptResponse, bool, error) {
	var result connectorAttemptResponse
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
		if _, err := tx.Exec(acceptCtx, `DELETE FROM client_connector_acceptances
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
		err := tx.QueryRow(acceptCtx, `INSERT INTO client_connector_acceptances
			(org_id, project_id, key_hash, payload_hash, receipt_id, expires_at)
			VALUES ($1,$2,$3,$4,$5,now()+($6::bigint * interval '1 microsecond'))
			ON CONFLICT (org_id,project_id,key_hash) DO NOTHING RETURNING receipt_id`,
			receipt.orgID, receipt.projectID, keyHash, payloadHash, receipt.id, clientConnectorKeyRetention.Microseconds()).Scan(&reservedID)
		if errors.Is(err, pgx.ErrNoRows) {
			rollbackCtx, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			rollbackErr := tx.Rollback(rollbackCtx)
			done()
			if rollbackErr != nil {
				return result, false, rollbackErr
			}
			lookupCtx, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			defer done()
			var existingHash string
			err = s.pool.QueryRow(lookupCtx, `SELECT a.payload_hash,r.id
				FROM client_connector_acceptances a JOIN ingest_receipts r
				  ON r.id=a.receipt_id AND r.org_id=a.org_id AND r.project_id=a.project_id
				WHERE a.org_id=$1 AND a.project_id=$2 AND a.key_hash=$3 AND a.expires_at>now()
				  AND r.completed_at IS NOT NULL AND r.outcome='accepted'
				  AND r.accepted_count=1 AND r.persisted_count=1 AND r.http_status=202`,
				receipt.orgID, receipt.projectID, keyHash).Scan(&existingHash, &result.ReceiptID)
			if err != nil {
				return connectorAttemptResponse{}, false, err
			}
			if existingHash != payloadHash {
				return connectorAttemptResponse{}, false, errClientConnectorKeyConflict
			}
			result.Accepted = true
			return result, true, nil
		}
		if err != nil {
			return result, false, err
		}
	}
	// A DSN holder can assert only app-observed health. It never attaches an
	// installed connection or promotes the result into server-observed data.
	params.OrgID = receipt.orgID
	params.ProjectID = pgtype.Text{String: receipt.projectID, Valid: true}
	params.ConnectionID = pgtype.Text{}
	params.SourceKind = "client_reported"
	if err := qtx.InsertConnectorAttempt(acceptCtx, params); err != nil {
		return connectorAttemptResponse{}, false, err
	}
	// InsertConnectorAttempt is a sqlc :exec query. Confirm the row exists in
	// this transaction so a future BEFORE INSERT trigger that suppresses a
	// row cannot create a terminal acceptance receipt for no observation.
	var storedID string
	if err := tx.QueryRow(acceptCtx, `SELECT id FROM connector_attempts
		WHERE id=$1 AND org_id=$2 AND project_id=$3
		  AND source_kind='client_reported' AND connection_id IS NULL`,
		params.ID, receipt.orgID, receipt.projectID).Scan(&storedID); err != nil {
		return connectorAttemptResponse{}, false, err
	}
	if _, err := qtx.CompleteIngestReceipt(acceptCtx, generated.CompleteIngestReceiptParams{
		ID: receipt.id, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		ReceivedCount: 1, AcceptedCount: 1, PersistedCount: 1,
		Outcome: "accepted", HttpStatus: http.StatusAccepted, Retryable: false,
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return connectorAttemptResponse{}, false, err
	}
	if err := tx.Commit(acceptCtx); err != nil {
		return connectorAttemptResponse{}, false, err
	}
	return connectorAttemptResponse{Accepted: true, ReceiptID: receipt.id}, false, nil
}

// Each replica sweeps at most 10,000 expired client retry identities every
// 30 seconds; connector observations and receipts remain untouched.
func (s *Server) RunClientConnectorAcceptancePrune(ctx context.Context) {
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
			pruned, err := s.pruneClientConnectorAcceptances(pruneCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("prune client connector retry keys", "error", err)
			} else if pruned == 10000 {
				slog.Warn("client connector retry key prune reached batch cap")
			}
		}
	}
}

func (s *Server) pruneClientConnectorAcceptances(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT org_id,project_id,key_hash FROM client_connector_acceptances
		WHERE expires_at <= now() ORDER BY expires_at LIMIT 10000 FOR UPDATE SKIP LOCKED
	)
	DELETE FROM client_connector_acceptances a USING expired e
	WHERE a.org_id=e.org_id AND a.project_id=e.project_id AND a.key_hash=e.key_hash`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
