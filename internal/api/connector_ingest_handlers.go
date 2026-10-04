package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

const maxConnectorElapsedMS int64 = 24 * 60 * 60 * 1000

// Provider quota headers are deliberately represented as bounded integers.
// The limits leave ample room for real provider plans while preventing an
// untrusted DSN sender from turning this health series into an arbitrary
// precision or long-lived payload store.
const (
	maxConnectorQuotaRemaining int64 = 1_000_000_000_000_000
	maxConnectorRetryAfterMS   int64 = 7 * 24 * 60 * 60 * 1000
	// A connector reports the age of the source watermark it actually observed
	// after a completed sync/read. Seven days bounds untrusted DSN metadata while
	// covering real webhook/polling backlogs.
	maxConnectorSyncLagMS int64 = 7 * 24 * 60 * 60 * 1000
)

var (
	connectorProviderToken    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,31}$`)
	connectorOperationToken   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	connectorErrorCodeToken   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	connectorConnectionToken  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	connectorStatusClassToken = regexp.MustCompile(`^[1-5]xx$`)
)

// connectorAttemptRequest is the small payload external applications send
// after calling a SaaS or internal provider. It contains no URL, response
// body, credential, user identifier, or provider payload.
type connectorAttemptRequest struct {
	Provider       string `json:"provider"`
	ConnectionID   string `json:"connection_id,omitempty"`
	Operation      string `json:"operation"`
	Channel        string `json:"channel"`
	Outcome        string `json:"outcome"`
	StatusClass    string `json:"status_class"`
	ErrorCode      string `json:"error_code"`
	ElapsedMS      int64  `json:"elapsed_ms"`
	QuotaRemaining *int64 `json:"quota_remaining,omitempty"`
	RetryAfterMS   *int64 `json:"retry_after_ms,omitempty"`
	SyncLagMS      *int64 `json:"sync_lag_ms,omitempty"`
}

type connectorAttemptResponse struct {
	Accepted  bool   `json:"accepted"`
	ReceiptID string `json:"receipt_id"`
}

// handleConnectorAttempt accepts one payload-free connector outcome from an
// external app. The project DSN authenticates the request; it cannot claim an
// org-owned provider installation or a server-observed provider result. The
// server derives timestamps, tenant/project scope, and client provenance.
func (s *Server) handleConnectorAttempt(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	requestOutcome := "rejected"
	defer func() {
		outcome.record("connector")
		recordIngestRequest("native", outcome, requestOutcome)
	}()

	project, ok := s.authIngest(w, r)
	if !ok {
		return
	}

	// Keep the exact bounded JSON bytes for an ambiguous-response retry. This
	// endpoint has never accepted gzip, so the serialized body is the identity.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var req connectorAttemptRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	normalized, err := normalizeConnectorAttemptRequest(req)
	if err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if normalized.ConnectionID != "" {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, "connection_id requires a server-side connector adapter")
		return
	}
	keyHash, err := nativeIngestKeyHash(r)
	if err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	receipt := newIngestReceipt(w, r, project, "connectors", "native")
	if receipt == nil || s.pool == nil {
		outcome.failed = 1
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	receipt.setReceived(1)

	completedAt := time.Now().UTC()
	params := generated.InsertConnectorAttemptParams{
		ID:             id.New(),
		OrgID:          project.OrgID,
		ProjectID:      pgtype.Text{String: project.ID, Valid: true},
		ConnectionID:   pgtype.Text{},
		SourceKind:     "client_reported",
		Provider:       normalized.Provider,
		Operation:      normalized.Operation,
		Channel:        normalized.Channel,
		Outcome:        normalized.Outcome,
		StatusClass:    normalized.StatusClass,
		ErrorCode:      normalized.ErrorCode,
		ElapsedMs:      normalized.ElapsedMS,
		QuotaRemaining: optionalInt8(normalized.QuotaRemaining),
		RetryAfterMs:   optionalInt8(normalized.RetryAfterMS),
		SyncLagMs:      optionalInt8(normalized.SyncLagMS),
		AttemptedAt:    pgtype.Timestamptz{Time: completedAt.Add(-time.Duration(normalized.ElapsedMS) * time.Millisecond), Valid: true},
		CompletedAt:    pgtype.Timestamptz{Time: completedAt, Valid: true},
	}
	result, replay, err := s.acceptClientConnectorAttempt(r.Context(), receipt, keyHash, nativeIngestPayloadHash(body), params)
	if err != nil {
		if err == errClientConnectorKeyConflict {
			outcome.invalid = 1
			writeErr(w, http.StatusConflict, "Idempotency-Key was already used for different connector payload bytes")
			return
		}
		outcome.failed = 1
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		slog.Error("client connector acceptance transaction failed", "project_id", project.ID)
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	if replay {
		outcome.deduplicated = 1
		w.Header().Set("Idempotency-Replayed", "true")
	} else {
		outcome.accepted = 1
	}
	requestOutcome = "accepted"
	w.Header().Set("X-Flare-Receipt-ID", result.ReceiptID)
	writeJSON(w, http.StatusAccepted, result)
}

func optionalInt8(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}

func normalizeConnectorAttemptRequest(req connectorAttemptRequest) (connectorAttemptRequest, error) {
	req.Provider = strings.TrimSpace(req.Provider)
	if !connectorProviderToken.MatchString(req.Provider) {
		return req, errors.New("provider is required and must be a safe bounded token")
	}
	req.ConnectionID = strings.TrimSpace(req.ConnectionID)
	if req.ConnectionID != "" && !connectorConnectionToken.MatchString(req.ConnectionID) {
		return req, errors.New("connection_id must be a safe bounded identifier")
	}
	req.Operation = strings.TrimSpace(req.Operation)
	if !connectorOperationToken.MatchString(req.Operation) {
		return req, errors.New("operation is required and must be a safe bounded token")
	}
	req.Channel = strings.TrimSpace(req.Channel)
	if !connectorProviderToken.MatchString(req.Channel) {
		return req, errors.New("channel is required and must be a safe bounded token")
	}
	req.Outcome = strings.ToLower(strings.TrimSpace(req.Outcome))
	if req.Outcome != "ok" && req.Outcome != "error" {
		return req, errors.New("outcome must be ok or error")
	}
	req.StatusClass = strings.ToLower(strings.TrimSpace(req.StatusClass))
	if req.StatusClass != "" && !connectorStatusClassToken.MatchString(req.StatusClass) {
		return req, errors.New("status_class must be an HTTP status class")
	}
	req.ErrorCode = strings.ToLower(strings.TrimSpace(req.ErrorCode))
	if req.ErrorCode != "" && !connectorErrorCodeToken.MatchString(req.ErrorCode) {
		return req, errors.New("error_code must be a safe bounded token")
	}
	if req.ElapsedMS < 0 || req.ElapsedMS > maxConnectorElapsedMS {
		return req, errors.New("elapsed_ms is outside the supported range")
	}
	if req.QuotaRemaining != nil && (*req.QuotaRemaining < 0 || *req.QuotaRemaining > maxConnectorQuotaRemaining) {
		return req, errors.New("quota_remaining is outside the supported range")
	}
	if req.RetryAfterMS != nil && (*req.RetryAfterMS < 0 || *req.RetryAfterMS > maxConnectorRetryAfterMS) {
		return req, errors.New("retry_after_ms is outside the supported range")
	}
	if req.SyncLagMS != nil && (*req.SyncLagMS < 0 || *req.SyncLagMS > maxConnectorSyncLagMS) {
		return req, errors.New("sync_lag_ms is outside the supported range")
	}
	if req.Outcome == "ok" {
		// An accepted call is always represented as a 2xx outcome. Ignore no
		// caller detail here: rejecting contradictory metadata keeps the stored
		// health series deterministic and payload-free.
		req.StatusClass = "2xx"
		req.ErrorCode = ""
		return req, nil
	}
	if req.StatusClass == "" && req.ErrorCode == "" {
		return req, errors.New("error outcome requires status_class or error_code")
	}
	return req, nil
}
