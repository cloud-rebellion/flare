package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/metrics"
)

const (
	// Receipt starts are part of the telemetry acceptance contract. A slow or
	// unavailable receipt table must make the sender retry before any payload is
	// persisted, so Flare never reports an accepted signal without provenance.
	ingestReceiptTimeout = 750 * time.Millisecond
	maxReceiptRequestID  = 128
)

type ingestReceipt struct {
	id        string
	orgID     string
	projectID string
	requestID pgtype.Text
	pillar    string
	protocol  string
	received  int64
}

// beginIngestReceipt creates a payload-free, tenant-scoped receipt after DSN
// authentication and body framing have succeeded. A nil result means the
// receipt could not be durably started; callers must return 503 before writing
// telemetry.
func (s *Server) beginIngestReceipt(ctx context.Context, w http.ResponseWriter, r *http.Request, project *generated.Project, pillar, protocol string) *ingestReceipt {
	receipt := newIngestReceipt(w, r, project, pillar, protocol)
	if receipt == nil || s.q == nil {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ingestReceiptTimeout)
	defer cancel()
	_, err := s.q.InsertIngestReceipt(writeCtx, generated.InsertIngestReceiptParams{
		ID:         receipt.id,
		RequestID:  receipt.requestID,
		OrgID:      receipt.orgID,
		ProjectID:  receipt.projectID,
		Pillar:     receipt.pillar,
		Protocol:   receipt.protocol,
		Outcome:    "in_progress",
		HttpStatus: http.StatusAccepted,
		Retryable:  true,
		ReceivedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		recordIngestReceiptFailure("insert", receipt)
		return nil
	}
	return receipt
}

// requireIngestReceipt starts the acceptance receipt and fails the request
// closed when the receipt store is unavailable. The outcome pointers keep the
// deferred accounting path aligned with the 503 response while the nil receipt
// ensures no completion write is attempted for a row that never existed.
func (s *Server) requireIngestReceipt(ctx context.Context, w http.ResponseWriter, r *http.Request, project *generated.Project, pillar, protocol string, outcome *ingestOutcome, requestOutcome *string, receiptStatus *int) *ingestReceipt {
	receipt := s.beginIngestReceipt(ctx, w, r, project, pillar, protocol)
	if receipt != nil {
		return receipt
	}
	if outcome != nil {
		outcome.failed++
	}
	if requestOutcome != nil {
		*requestOutcome = "failed"
	}
	if receiptStatus != nil {
		*receiptStatus = http.StatusServiceUnavailable
	}
	writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
	return nil
}

func newIngestReceipt(w http.ResponseWriter, r *http.Request, project *generated.Project, pillar, protocol string) *ingestReceipt {
	if project == nil {
		return nil
	}
	return &ingestReceipt{
		id:        id.New(),
		orgID:     project.OrgID,
		projectID: project.ID,
		requestID: receiptRequestID(w, r),
		pillar:    canonicalReceiptPillar(pillar),
		protocol:  canonicalReceiptProtocol(protocol),
	}
}

// finishIngestReceipt closes a receipt after the telemetry handler has decided
// its wire outcome. It uses a detached short deadline so a client disconnect or
// request cancellation cannot prevent durable accounting from being attempted.
func (s *Server) finishIngestReceipt(ctx context.Context, receipt *ingestReceipt, outcome ingestOutcome, forced string, status int) {
	if receipt == nil || s.q == nil {
		return
	}
	if status < http.StatusContinue || status > 599 {
		status = http.StatusInternalServerError
	}
	result := forced
	if result == "" {
		result = classifyIngestRequest(outcome)
	}
	result = canonicalReceiptOutcome(result)
	retryable := result == "failed"
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ingestReceiptTimeout)
	defer cancel()
	_, err := s.q.CompleteIngestReceipt(writeCtx, generated.CompleteIngestReceiptParams{
		ID:             receipt.id,
		OrgID:          receipt.orgID,
		ProjectID:      receipt.projectID,
		ReceivedCount:  receipt.received,
		AcceptedCount:  int64(outcome.accepted),
		RejectedCount:  int64(outcome.invalid + outcome.failed),
		DroppedCount:   int64(outcome.dropped),
		PersistedCount: int64(outcome.accepted),
		Outcome:        result,
		HttpStatus:     int16(status),
		Retryable:      retryable,
		CompletedAt:    pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		recordIngestReceiptFailure("complete", receipt)
	}
}

func (r *ingestReceipt) setReceived(n int) {
	if r == nil || n <= 0 {
		return
	}
	// All parser and budget caps are far below int64, but keep this boundary
	// fail-closed if a future parser changes its count type.
	if int64(n) < 0 {
		r.received = int64(^uint64(0) >> 1)
		return
	}
	r.received = int64(n)
}

func receiptRequestID(w http.ResponseWriter, r *http.Request) pgtype.Text {
	// chi's request ID is generated by trusted middleware. Prefer it over a
	// caller-supplied header so a tenant cannot choose another request's receipt
	// key; fall back to the response header for deployments that expose it.
	var raw string
	if r != nil {
		raw = chimw.GetReqID(r.Context())
		if raw == "" {
			raw = r.Header.Get("X-Request-ID")
		}
	}
	if raw == "" && w != nil {
		raw = w.Header().Get("X-Request-ID")
	}
	raw = sanitizeReceiptRequestID(raw)
	if raw == "" {
		raw = id.New()
	}
	return pgtype.Text{String: raw, Valid: true}
}

func sanitizeReceiptRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.:", r) {
			if b.Len()+utf8.RuneLen(r) > maxReceiptRequestID {
				break
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func canonicalReceiptPillar(pillar string) string {
	switch pillar {
	case "envelope", "event", "logs", "spans", "metrics", "connectors", "business", "symbols":
		return pillar
	default:
		return "other"
	}
}

func canonicalReceiptProtocol(protocol string) string {
	switch protocol {
	case "sentry", "otlp", "native":
		return protocol
	default:
		return "other"
	}
}

func canonicalReceiptOutcome(outcome string) string {
	switch outcome {
	case "in_progress", "accepted", "partial", "rejected", "failed", "empty":
		return outcome
	default:
		return "failed"
	}
}

func recordIngestReceiptFailure(operation string, receipt *ingestReceipt) {
	if receipt == nil {
		return
	}
	metrics.Inc("flare_ingest_receipts", map[string]string{"operation": operation, "outcome": "error"})
	slog.Warn("ingest receipt write failed",
		"operation", operation,
		"project_id", receipt.projectID,
		"org_id", receipt.orgID,
		"request_id", receipt.requestID.String,
	)
}
