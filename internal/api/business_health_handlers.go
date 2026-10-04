package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

const (
	maxBusinessElapsedMS       = int64(24 * 60 * 60 * 1000)
	defaultBusinessHealthHours = 24
	maxBusinessHealthHours     = 720
	defaultBusinessHealthLimit = 100
	maxBusinessHealthLimit     = 100
	minBusinessIdempotencyKey  = 16
	maxBusinessIdempotencyKey  = 128
)

var (
	businessCheckNameToken   = regexp.MustCompile(`^[a-z][a-z0-9._:-]{0,63}$`)
	businessErrorCodeToken   = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)
	businessStatusClassToken = regexp.MustCompile(`^[1-5]xx$`)
	businessIdempotencyToken = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	businessBearerToken      = regexp.MustCompile(`^flb_[0-9a-f]{48}$`)
)

// businessCheckRequest is the complete public contract for a business
// outcome. It intentionally has no payload, URL, user, account, or provider
// fields. The confidential project key supplies tenant and project provenance.
type businessCheckRequest struct {
	CheckName   string `json:"check_name"`
	Outcome     string `json:"outcome"`
	ElapsedMS   *int64 `json:"elapsed_ms"`
	StatusClass string `json:"status_class"`
	ErrorCode   string `json:"error_code"`
}

type businessCheckResponse struct {
	Accepted  bool   `json:"accepted"`
	ReceiptID string `json:"receipt_id"`
}

// businessCheckStatusWriter captures the final HTTP classification for one
// tenant-known attempt. The handler only uses ordinary JSON responses.
type businessCheckStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *businessCheckStatusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *businessCheckStatusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

type businessHealthResponse struct {
	CheckName       string     `json:"check_name"`
	Attempts        int64      `json:"attempts"`
	OKCount         int64      `json:"ok_count"`
	ErrorCount      int64      `json:"error_count"`
	ErrorRate       float64    `json:"error_rate"`
	AvgElapsedMS    float64    `json:"avg_elapsed_ms"`
	MaxElapsedMS    int64      `json:"max_elapsed_ms"`
	LastCompletedAt *time.Time `json:"last_completed_at,omitempty"`
	LastOKAt        *time.Time `json:"last_ok_at,omitempty"`
	LastErrorAt     *time.Time `json:"last_error_at,omitempty"`
}

func normalizeBusinessCheckRequest(req businessCheckRequest) (businessCheckRequest, error) {
	req.CheckName = strings.ToLower(strings.TrimSpace(req.CheckName))
	if !businessCheckNameToken.MatchString(req.CheckName) {
		return req, errors.New("check_name is required and must be a bounded lowercase token")
	}
	req.Outcome = strings.ToLower(strings.TrimSpace(req.Outcome))
	if req.Outcome != "ok" && req.Outcome != "error" {
		return req, errors.New("outcome must be ok or error")
	}
	if req.ElapsedMS == nil || *req.ElapsedMS < 0 || *req.ElapsedMS > maxBusinessElapsedMS {
		return req, errors.New("elapsed_ms is outside the supported range")
	}
	req.StatusClass = strings.ToLower(strings.TrimSpace(req.StatusClass))
	if req.StatusClass != "" && !businessStatusClassToken.MatchString(req.StatusClass) {
		return req, errors.New("status_class must be an HTTP status class")
	}
	req.ErrorCode = strings.ToLower(strings.TrimSpace(req.ErrorCode))
	if req.ErrorCode != "" && !businessErrorCodeToken.MatchString(req.ErrorCode) {
		return req, errors.New("error_code must be a bounded lowercase token")
	}
	if req.Outcome == "ok" {
		// Normalize successful assertions to a stable representation. Caller
		// supplied failure metadata cannot turn a successful business outcome
		// into an error-rate point.
		req.StatusClass = "2xx"
		req.ErrorCode = ""
		return req, nil
	}
	if req.StatusClass == "2xx" {
		return req, errors.New("error outcome cannot use status_class 2xx")
	}
	if req.StatusClass == "" && req.ErrorCode == "" {
		return req, errors.New("error outcome requires status_class or error_code")
	}
	return req, nil
}

// A retry key is scoped by the authenticated project. Neither the raw key nor
// the assertion content is retained; the hash and normalized fingerprint are
// the only values needed to resolve an ambiguous HTTP response.
func businessIdempotencyKeyHash(r *http.Request) (string, error) {
	if r == nil {
		return "", errors.New("Idempotency-Key is required")
	}
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || len(values[0]) < minBusinessIdempotencyKey || len(values[0]) > maxBusinessIdempotencyKey || !businessIdempotencyToken.MatchString(values[0]) {
		return "", errors.New("Idempotency-Key must be one opaque 16-128 character token")
	}
	sum := sha256.Sum256([]byte(values[0]))
	return hex.EncodeToString(sum[:]), nil
}

func businessPayloadHash(req businessCheckRequest) string {
	// All token fields reject NUL, so these separators make the five fields
	// unambiguous. req is normalized before this function is called.
	canonical := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s", req.CheckName, req.Outcome, *req.ElapsedMS, req.StatusClass, req.ErrorCode)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// handleBusinessCheck accepts one business assertion and commits the
// payload-free check plus its accepted ingest receipt in one transaction.
// Returning the receipt id gives an SDK a durable acceptance handle.
func (s *Server) handleBusinessCheck(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	defer func() { outcome.record("business") }()
	project, businessKeyID, ok := s.authBusinessCheck(w, r)
	if !ok {
		recordIngestRequest("native", outcome, "rejected")
		return
	}
	usageWriter := &businessCheckStatusWriter{ResponseWriter: w}
	w = usageWriter
	defer func() {
		delta := usageDelta{
			OrgID: project.OrgID, ProjectID: project.ID, CredentialID: businessKeyID,
			Surface: "business_key", Operation: "business_checks", Requests: 1,
		}
		if usageWriter.status != http.StatusAccepted {
			delta.Errors = 1
		}
		if usageWriter.status == http.StatusUnauthorized {
			delta.Denied = 1
		}
		s.recordUsage(r.Context(), delta)
	}()
	var req businessCheckRequest
	if err := decodeJSON(r, &req); err != nil {
		outcome.invalid = 1
		recordIngestRequest("native", outcome, "rejected")
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	normalized, err := normalizeBusinessCheckRequest(req)
	if err != nil {
		outcome.invalid = 1
		recordIngestRequest("native", outcome, "rejected")
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	keyHash, err := businessIdempotencyKeyHash(r)
	if err != nil {
		outcome.invalid = 1
		recordIngestRequest("native", outcome, "rejected")
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	payloadHash := businessPayloadHash(normalized)
	if s.pool == nil {
		outcome.failed = 1
		recordIngestRequest("native", outcome, "failed")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	receipt := newIngestReceipt(w, r, project, "business", "native")
	if receipt == nil {
		outcome.failed = 1
		recordIngestRequest("native", outcome, "failed")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	receipt.setReceived(1)
	now := time.Now().UTC()
	// A concurrent same-key insert waits on the unique index. Bound the whole
	// provisional receipt/check/commit transaction so a stalled database yields
	// a retryable 503; the same key resolves an ambiguous commit on retry.
	acceptCtx, cancelAcceptance := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancelAcceptance()
	tx, err := s.pool.Begin(acceptCtx)
	if err != nil {
		outcome.failed = 1
		recordIngestRequest("native", outcome, "failed")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanupCtx)
	}()
	qtx := generated.New(tx)
	if _, lockErr := qtx.LockActiveBusinessCheckKey(acceptCtx, generated.LockActiveBusinessCheckKeyParams{
		ID: businessKeyID, OrgID: project.OrgID, ProjectID: project.ID,
	}); lockErr != nil {
		if errors.Is(lockErr, pgx.ErrNoRows) {
			recordIngestRequest("native", outcome, "rejected")
			writeErr(w, http.StatusUnauthorized, "valid business check key required")
			return
		}
		outcome.failed = 1
		recordIngestRequest("native", outcome, "failed")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	if _, err = qtx.InsertIngestReceipt(acceptCtx, generated.InsertIngestReceiptParams{
		ID: receipt.id, RequestID: receipt.requestID, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		Pillar: receipt.pillar, Protocol: receipt.protocol, Outcome: "in_progress", HttpStatus: http.StatusAccepted,
		Retryable: true, ReceivedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		outcome.failed = 1
		recordIngestRequest("native", outcome, "failed")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	_, err = qtx.InsertBusinessCheck(acceptCtx, generated.InsertBusinessCheckParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, ReceiptID: receipt.id,
		BusinessKeyID:      businessKeyID,
		IdempotencyKeyHash: keyHash, PayloadHash: payloadHash,
		CheckName: normalized.CheckName, Outcome: normalized.Outcome, StatusClass: normalized.StatusClass,
		ErrorCode: normalized.ErrorCode, ElapsedMs: *normalized.ElapsedMS,
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT waits for any concurrent attempt to commit. Drop the
		// provisional receipt, then read the winning, committed observation.
		rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		err = tx.Rollback(rollbackCtx)
		cancelRollback()
		if err != nil {
			outcome.failed = 1
			recordIngestRequest("native", outcome, "failed")
			writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
			return
		}
		lookupCtx, cancelLookup := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		existing, lookupErr := s.q.GetBusinessCheckByIdempotencyKey(lookupCtx, generated.GetBusinessCheckByIdempotencyKeyParams{
			OrgID: project.OrgID, ProjectID: project.ID, IdempotencyKeyHash: keyHash,
		})
		cancelLookup()
		if lookupErr != nil {
			outcome.failed = 1
			recordIngestRequest("native", outcome, "failed")
			writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
			return
		}
		if existing.PayloadHash != payloadHash {
			outcome.invalid = 1
			recordIngestRequest("native", outcome, "rejected")
			writeErr(w, http.StatusConflict, "Idempotency-Key was already used for a different business check")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		recordIngestRequest("native", outcome, "accepted")
		writeJSON(w, http.StatusAccepted, businessCheckResponse{Accepted: true, ReceiptID: existing.ReceiptID})
		return
	}
	if err != nil {
		outcome.failed = 1
		recordIngestRequest("native", outcome, "failed")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	if _, err = qtx.CompleteIngestReceipt(acceptCtx, generated.CompleteIngestReceiptParams{
		ID: receipt.id, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		ReceivedCount: 1, AcceptedCount: 1, PersistedCount: 1, Outcome: "accepted",
		HttpStatus: http.StatusAccepted, Retryable: false,
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		outcome.failed = 1
		recordIngestRequest("native", outcome, "failed")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	if err = tx.Commit(acceptCtx); err != nil {
		outcome.failed = 1
		recordIngestRequest("native", outcome, "failed")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	outcome.accepted = 1
	recordIngestRequest("native", outcome, "accepted")
	writeJSON(w, http.StatusAccepted, businessCheckResponse{Accepted: true, ReceiptID: receipt.id})
}

// A browser DSN never authorizes an assertion that can drive a business
// incident. This lookup binds a confidential, separately provisioned bearer
// key to the path project and returns the same 401 for absent, revoked,
// expired, wrong-project, and unknown keys.
func (s *Server) authBusinessCheck(w http.ResponseWriter, r *http.Request) (*generated.Project, string, bool) {
	values := r.Header.Values("Authorization")
	keyText := bearerToken(r)
	if len(values) != 1 || !businessBearerToken.MatchString(keyText) {
		writeErr(w, http.StatusUnauthorized, "valid business check key required")
		return nil, "", false
	}
	if s.q == nil {
		writeErr(w, http.StatusServiceUnavailable, "business check authentication is temporarily unavailable")
		return nil, "", false
	}
	key, err := s.q.GetBusinessCheckKeyForIngest(r.Context(), generated.GetBusinessCheckKeyForIngestParams{
		KeyHash: auth.HashAPIKey(keyText), ID: chi.URLParam(r, "projectID"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusUnauthorized, "valid business check key required")
		return nil, "", false
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "business check authentication is temporarily unavailable")
		return nil, "", false
	}
	return &generated.Project{ID: key.ProjectID, OrgID: key.OrgID}, key.BusinessKeyID, true
}

// handleListBusinessHealth returns aggregate outcomes for a project while
// enforcing both project and authenticated organization ownership.
func (s *Server) handleListBusinessHealth(w http.ResponseWriter, r *http.Request) {
	project, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context())})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	since := time.Now().UTC().Add(-time.Duration(businessHealthHours(r)) * time.Hour)
	rows, err := s.q.ListBusinessHealthByProject(r.Context(), generated.ListBusinessHealthByProjectParams{
		ProjectID: project.ID, OrgID: project.OrgID, Since: pgtype.Timestamptz{Time: since, Valid: true}, RowLimit: int32(businessHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list business health", err)
		return
	}
	out := make([]businessHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		mapped := businessHealthResponse{
			CheckName: row.CheckName, Attempts: row.Attempts, OKCount: row.OkCount, ErrorCount: row.ErrorCount,
			AvgElapsedMS: row.AvgElapsedMs, MaxElapsedMS: row.MaxElapsedMs,
			LastCompletedAt: receiptTimestamp(row.LastCompletedAt), LastOKAt: receiptTimestamp(row.LastOkAt), LastErrorAt: receiptTimestamp(row.LastErrorAt),
		}
		if mapped.Attempts > 0 {
			mapped.ErrorRate = float64(mapped.ErrorCount) / float64(mapped.Attempts)
		}
		out = append(out, mapped)
	}
	writeJSON(w, http.StatusOK, out)
}

func businessHealthHours(r *http.Request) int {
	v := 0
	if r != nil {
		v, _ = strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("hours")))
	}
	if v <= 0 {
		return defaultBusinessHealthHours
	}
	if v > maxBusinessHealthHours {
		return maxBusinessHealthHours
	}
	return v
}

func businessHealthLimit(r *http.Request) int {
	v := 0
	if r != nil {
		v, _ = strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	}
	if v <= 0 {
		return defaultBusinessHealthLimit
	}
	if v > maxBusinessHealthLimit {
		return maxBusinessHealthLimit
	}
	return v
}

func businessHealthReason(stats *generated.GetBusinessHealthStatsRow, checkName, comparison string, threshold float64, windowMinutes int32) string {
	if stats == nil || stats.Attempts == 0 || stats.ErrorCount == 0 {
		return ""
	}
	rate := float64(stats.ErrorCount) / float64(stats.Attempts)
	fire := comparison == "gt" && rate > threshold || comparison != "gt" && rate >= threshold
	if !fire {
		return ""
	}
	return fmt.Sprintf("Business check %s error rate %.2f%% reached %.2f%% over %dm (%d/%d errors)", checkName, rate*100, threshold*100, windowMinutes, stats.ErrorCount, stats.Attempts)
}
