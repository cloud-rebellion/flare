package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	Accepted bool `json:"accepted"`
}

// handleConnectorAttempt accepts one payload-free connector outcome from an
// external app. The project DSN authenticates the request; the server derives
// both timestamps and tenant/project scope, then writes the durable attempt
// before acknowledging it.
func (s *Server) handleConnectorAttempt(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	var receipt *ingestReceipt
	receiptStatus := http.StatusAccepted
	requestOutcome := "rejected"
	defer func() {
		outcome.record("connector")
		recordIngestRequest("native", outcome, requestOutcome)
		s.finishIngestReceipt(r.Context(), receipt, outcome, requestOutcome, receiptStatus)
	}()

	project, ok := s.authIngest(w, r)
	if !ok {
		return
	}

	var req connectorAttemptRequest
	if err := decodeJSON(r, &req); err != nil {
		outcome.invalid = 1
		receiptStatus = http.StatusBadRequest
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	normalized, err := normalizeConnectorAttemptRequest(req)
	if err != nil {
		outcome.invalid = 1
		receiptStatus = http.StatusBadRequest
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	connectionID := pgtype.Text{}
	if normalized.ConnectionID != "" {
		connection, connectionErr := s.q.GetConnectorConnectionByID(r.Context(), generated.GetConnectorConnectionByIDParams{
			ID: normalized.ConnectionID, OrgID: project.OrgID,
		})
		if errors.Is(connectionErr, pgx.ErrNoRows) {
			outcome.invalid = 1
			receiptStatus = http.StatusBadRequest
			writeErr(w, http.StatusBadRequest, "connection_id is not registered in this workspace")
			return
		}
		if connectionErr != nil {
			outcome.failed = 1
			receiptStatus = http.StatusServiceUnavailable
			slogError(w, "resolve connector connection", connectionErr)
			return
		}
		if !strings.EqualFold(connection.Provider, normalized.Provider) {
			outcome.invalid = 1
			receiptStatus = http.StatusBadRequest
			writeErr(w, http.StatusBadRequest, "connection_id provider does not match provider")
			return
		}
		connectionID = pgtype.Text{String: normalized.ConnectionID, Valid: true}
	}
	receipt = s.requireIngestReceipt(r.Context(), w, r, project, "connectors", "native", &outcome, &requestOutcome, &receiptStatus)
	if receipt == nil {
		return
	}
	receipt.setReceived(1)

	completedAt := time.Now().UTC()
	if err := s.q.InsertConnectorAttempt(r.Context(), generated.InsertConnectorAttemptParams{
		ID:             id.New(),
		OrgID:          project.OrgID,
		ProjectID:      pgtype.Text{String: project.ID, Valid: true},
		ConnectionID:   connectionID,
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
	}); err != nil {
		outcome.failed = 1
		requestOutcome = "failed"
		receiptStatus = http.StatusServiceUnavailable
		slogError(w, "record external connector attempt", err)
		return
	}

	outcome.accepted = 1
	requestOutcome = ""
	writeJSON(w, http.StatusAccepted, connectorAttemptResponse{Accepted: true})
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
