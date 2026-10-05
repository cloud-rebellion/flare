package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/metrics"
	"github.com/bright-interaction/flare/internal/telemetry"
)

const aiActionReceiptTimeout = 750 * time.Millisecond

// errAIActionReceiptUnavailable is returned before an AI request is started
// when Flare cannot durably record its acceptance receipt. Callers should
// retry: proceeding would make a triage result untraceable to its evidence.
var errAIActionReceiptUnavailable = errors.New("AI action receipt is temporarily unavailable; retry the request")

type aiActionReceipt struct {
	id        string
	orgID     string
	projectID string
	issueID   string
	operation string
	provider  string
	// evidenceDigest fingerprints the bounded, scrubbed incident context used
	// for a fresh provider call. It remains empty for cached reads.
	evidenceDigest string
}

// beginAIActionReceipt records the start of a bounded AI operation. A nil
// result means the operation must not proceed because its durable acceptance
// receipt could not be written. The row intentionally contains no prompt,
// completion, credentials, or provider error text.
func (s *Server) beginAIActionReceipt(ctx context.Context, org, project, issue, operation, provider string) *aiActionReceipt {
	if s.q == nil || org == "" || project == "" || issue == "" {
		return nil
	}
	r := &aiActionReceipt{
		id:        id.New(),
		orgID:     org,
		projectID: project,
		issueID:   issue,
		operation: canonicalAIActionOperation(operation),
		provider:  canonicalAIActionProvider(provider),
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), aiActionReceiptTimeout)
	defer cancel()
	inserted, err := s.q.InsertAIActionReceipt(writeCtx, generated.InsertAIActionReceiptParams{
		ID:             r.id,
		OrgID:          r.orgID,
		ProjectID:      r.projectID,
		IssueID:        r.issueID,
		Operation:      r.operation,
		Status:         "started",
		Provider:       r.provider,
		StartedAt:      pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		Retryable:      true,
		EvidenceDigest: r.evidenceDigest,
	})
	if err != nil {
		recordAIActionReceiptFailure("insert", r)
		return nil
	}
	// Return the ID confirmed by the durable insert, rather than relying on
	// the requested ID remaining equal to the stored row's identifier.
	r.id = inserted.ID
	recordAIActionReceiptSuccess("insert")
	return r
}

// requireAIActionReceipt starts the receipt and turns an unavailable receipt
// store into a typed, retryable error for the triage caller.
func (s *Server) requireAIActionReceipt(ctx context.Context, org, project, issue, operation, provider string) (*aiActionReceipt, error) {
	receipt := s.beginAIActionReceipt(ctx, org, project, issue, operation, provider)
	if receipt == nil {
		return nil, errAIActionReceiptUnavailable
	}
	return receipt, nil
}

// finishAIActionReceipt closes an AI action with a fixed status and safe error
// code. The detached deadline lets a client disconnect without losing the
// durable outcome. It is deliberately best-effort and never changes the
// triage result returned to the caller.
func (s *Server) finishAIActionReceipt(ctx context.Context, r *aiActionReceipt, cached bool, actionErr error) {
	if r == nil || s.q == nil {
		return
	}
	status, retryable, errorCode := classifyAIActionReceipt(cached, actionErr)
	errorText := pgtype.Text{}
	if errorCode != "" {
		errorText = pgtype.Text{String: errorCode, Valid: true}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), aiActionReceiptTimeout)
	defer cancel()
	_, err := s.q.CompleteAIActionReceipt(writeCtx, generated.CompleteAIActionReceiptParams{
		ID:             r.id,
		OrgID:          r.orgID,
		ProjectID:      r.projectID,
		Status:         status,
		Provider:       canonicalAIActionProvider(r.provider),
		CompletedAt:    timeStampNow(),
		Retryable:      retryable,
		ErrorCode:      errorText,
		EvidenceDigest: r.evidenceDigest,
	})
	if err != nil {
		recordAIActionReceiptFailure("complete", r)
		return
	}
	recordAIActionReceiptSuccess("complete")
}

// aiEvidenceDigest returns a stable SHA-256 fingerprint for the bounded,
// scrubbed report sent inside the per-request untrusted-data fence. The report
// itself is never persisted; the digest lets an operator prove which evidence
// snapshot a receipt represents without retaining prompts or telemetry.
func aiEvidenceDigest(report string) string {
	sum := sha256.Sum256([]byte(report))
	return hex.EncodeToString(sum[:])
}

func classifyAIActionReceipt(cached bool, actionErr error) (status string, retryable bool, errorCode string) {
	if actionErr == nil {
		if cached {
			return "cached", false, ""
		}
		return "succeeded", false, ""
	}
	switch {
	case errors.Is(actionErr, errAIActionReceiptUnavailable):
		return "failed", true, "receipt_unavailable"
	case errors.Is(actionErr, errTriageEndpoint):
		return "failed", true, "provider_error"
	case errors.Is(actionErr, errTriageBudget):
		return "failed", true, "budget_exhausted"
	case errors.Is(actionErr, errTriageNotConfigured):
		return "failed", false, "not_configured"
	default:
		var notFound telemetry.ErrNotFound
		if errors.As(actionErr, &notFound) {
			return "failed", false, "issue_not_found"
		}
		return "failed", true, "internal"
	}
}

func canonicalAIActionOperation(operation string) string {
	if operation == "triage" {
		return operation
	}
	return "other"
}

func canonicalAIActionProvider(provider string) string {
	switch provider {
	case "openai", "anthropic", "cache", "unknown":
		return provider
	default:
		return "other"
	}
}

func timeStampNow() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
}

func recordAIActionReceiptFailure(operation string, r *aiActionReceipt) {
	if r == nil {
		return
	}
	metrics.Inc("flare_ai_action_receipts", map[string]string{"operation": operation, "outcome": "error"})
	slog.Warn("ai action receipt write failed",
		"operation", operation,
		"org_id", r.orgID,
		"project_id", r.projectID,
		"issue_id", r.issueID,
	)
}

// recordAIActionReceiptSuccess keeps the receipt write counter useful for
// throughput and drop-rate queries without adding tenant, issue, provider, or
// error text to the process-wide metrics surface. The operation values are
// fixed by the two receipt write sites above.
func recordAIActionReceiptSuccess(operation string) {
	metrics.Inc("flare_ai_action_receipts", map[string]string{"operation": operation, "outcome": "ok"})
}
