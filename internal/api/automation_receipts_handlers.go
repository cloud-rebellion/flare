package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

const (
	defaultAutomationReceiptLimit = 100
	maxAutomationReceiptLimit     = 500
	maxAutomationNextReceiptRows  = 500
	maxAutomationReceiptIssueID   = 128
	maxAutomationReceiptReference = 256
	maxAutomationReceiptErrorCode = 64
	maxAutomationIdempotencyKey   = 128
)

var (
	automationReceiptStages = map[string]struct{}{
		"triage": {}, "patch": {}, "review": {}, "ci": {},
		"merge": {}, "deploy": {}, "rollback": {}, "synthetic": {},
	}
	automationReceiptStatuses = map[string]struct{}{
		"pending": {}, "approved": {}, "passed": {}, "failed": {}, "blocked": {},
	}
)

// Completed workflow evidence is ordered by the revision it advances. A
// receipt endpoint must not let a caller mint a "deploy passed" row without a
// human-approved review and the preceding CI/merge receipts for that same
// revision. Failed or blocked stages remain recordable without a predecessor
// so the workflow can explain where it stopped.
var automationReceiptPrerequisites = map[string]struct {
	stage  string
	status string
}{
	"review:approved":  {stage: "patch", status: "passed"},
	"ci:passed":        {stage: "review", status: "approved"},
	"merge:passed":     {stage: "ci", status: "passed"},
	"deploy:passed":    {stage: "merge", status: "passed"},
	"synthetic:passed": {stage: "deploy", status: "passed"},
}

type createAutomationReceiptRequest struct {
	IssueID        string `json:"issue_id"`
	Stage          string `json:"stage"`
	Status         string `json:"status"`
	IdempotencyKey string `json:"idempotency_key"`
	ClaimID        string `json:"claim_id"`
	EvidenceRef    string `json:"evidence_ref"`
	SourceRevision string `json:"source_revision"`
	TargetRevision string `json:"target_revision"`
	RunRef         string `json:"run_ref"`
	Retryable      bool   `json:"retryable"`
	ErrorCode      string `json:"error_code"`
}

// automationReceiptResponse intentionally has no prompts, completions,
// credentials, source contents, or provider error text. References are bounded
// opaque identifiers supplied by the caller and are never dereferenced here.
type automationReceiptResponse struct {
	ID                 string     `json:"id"`
	IssueID            string     `json:"issue_id"`
	Stage              string     `json:"stage"`
	Status             string     `json:"status"`
	VerificationStatus string     `json:"verification_status"`
	IdempotencyKey     string     `json:"idempotency_key"`
	EvidenceRef        string     `json:"evidence_ref,omitempty"`
	SourceRevision     string     `json:"source_revision,omitempty"`
	TargetRevision     string     `json:"target_revision,omitempty"`
	RunRef             string     `json:"run_ref,omitempty"`
	ActorUserID        string     `json:"actor_user_id,omitempty"`
	Retryable          bool       `json:"retryable"`
	ErrorCode          string     `json:"error_code,omitempty"`
	CreatedAt          *time.Time `json:"created_at,omitempty"`
	UpdatedAt          *time.Time `json:"updated_at,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

// automationNextResponse is the read-only handoff from Flare to an external
// coding-loop worker. It contains only the stage that is eligible after the
// latest bounded receipt for this issue and revision; Flare never executes the
// returned action or dereferences any reference.
type automationNextResponse struct {
	Available             bool   `json:"available"`
	IssueID               string `json:"issue_id"`
	TargetRevision        string `json:"target_revision,omitempty"`
	Stage                 string `json:"stage,omitempty"`
	State                 string `json:"state"`
	PrerequisiteReceiptID string `json:"prerequisite_receipt_id,omitempty"`
}

type automationVerificationResponse struct {
	State          string `json:"state"`
	WindowHours    int    `json:"window_hours"`
	PendingCount   int64  `json:"pending_count"`
	CICount        int64  `json:"ci_count"`
	MergeCount     int64  `json:"merge_count"`
	DeployCount    int64  `json:"deploy_count"`
	SyntheticCount int64  `json:"synthetic_count"`
}

var automationWorkflowStages = []string{"patch", "review", "ci", "merge", "deploy", "synthetic"}

func (s *Server) handleCreateAutomationReceipt(w http.ResponseWriter, r *http.Request) {
	var req createAutomationReceiptRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	normalized, err := normalizeAutomationReceiptRequest(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if normalized.Status == "approved" && userIDFrom(r.Context()) == "" {
		// API keys can be member-scoped but carry no human identity. They may
		// record pending/results, but an approval must always name the session
		// actor that authorized it.
		writeErr(w, http.StatusForbidden, "approved receipt requires an authenticated user actor")
		return
	}
	if normalized.Status == "passed" && automationClaimStage(normalized.Stage) && normalized.ClaimID == "" {
		// A successful worker stage must be bound to an atomic claim. Failed
		// and pending receipts remain recordable without one for backwards
		// compatible diagnostics, but they cannot advance the workflow.
		writeErr(w, http.StatusConflict, "passed execution receipts require an active claim")
		return
	}

	projectID := chi.URLParam(r, "id")
	orgID := orgIDFrom(r.Context())
	project, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: projectID, OrgScope: orgID})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	// Resolve the issue before inserting so a missing or cross-project issue
	// cannot be mistaken for an idempotency conflict when the scoped INSERT
	// returns no row.
	issue, err := s.q.GetIssue(r.Context(), generated.GetIssueParams{ID: normalized.IssueID, OrgID: orgID})
	if err != nil || issue == nil || issue.ProjectID != project.ID {
		writeErr(w, http.StatusNotFound, "issue not found")
		return
	}
	if normalized.Stage == "patch" && normalized.Status == "passed" {
		exists, checkErr := s.q.HasSuccessfulAIActionEvidence(r.Context(), generated.HasSuccessfulAIActionEvidenceParams{
			OrgID: project.OrgID, ProjectID: project.ID, IssueID: normalized.IssueID,
			EvidenceDigest: normalized.EvidenceRef,
		})
		if checkErr != nil {
			slogError(w, "check AI evidence for patch receipt", checkErr)
			return
		}
		if !exists {
			writeErr(w, http.StatusConflict, "patch evidence is not a completed AI triage receipt for this issue")
			return
		}
	}
	if prerequisite, ok := automationReceiptPrerequisite(normalized); ok {
		exists, checkErr := s.q.HasAutomationReceiptEvidence(r.Context(), generated.HasAutomationReceiptEvidenceParams{
			OrgID: project.OrgID, ProjectID: project.ID, IssueID: normalized.IssueID,
			Stage: prerequisite.stage, Status: prerequisite.status, TargetRevision: normalized.TargetRevision,
		})
		if checkErr != nil {
			slogError(w, "check automation receipt prerequisite", checkErr)
			return
		}
		if !exists {
			writeErr(w, http.StatusConflict, "workflow prerequisite is not satisfied for this target revision")
			return
		}
	}

	actor := pgtype.Text{}
	if uid := userIDFrom(r.Context()); uid != "" {
		actor = pgText(uid)
	}
	var completedAt pgtype.Timestamptz
	if automationReceiptTerminal(normalized.Status) {
		completedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	row, err := s.q.InsertAutomationReceipt(r.Context(), generated.InsertAutomationReceiptParams{
		ID:             id.New(),
		OrgID:          project.OrgID,
		ProjectID:      project.ID,
		IssueID:        normalized.IssueID,
		Stage:          normalized.Stage,
		Status:         normalized.Status,
		IdempotencyKey: normalized.IdempotencyKey,
		EvidenceRef:    normalized.EvidenceRef,
		SourceRevision: normalized.SourceRevision,
		TargetRevision: normalized.TargetRevision,
		RunRef:         normalized.RunRef,
		ActorUserID:    actor,
		Retryable:      normalized.Retryable,
		ErrorCode:      normalized.ErrorCode,
		CompletedAt:    completedAt,
		ClaimID:        normalized.ClaimID,
	})
	if err == nil {
		s.audit(r.Context(), "automation.receipt.create", normalized.IssueID)
		writeJSON(w, http.StatusCreated, toInsertedAutomationReceiptResponse(row))
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slogError(w, "create automation receipt", err)
		return
	}

	// INSERT ... ON CONFLICT DO NOTHING makes retries race-safe. A duplicate
	// key is only idempotent when every immutable field matches; silently
	// accepting a changed body would turn a key reuse into an audit forgery.
	existing, getErr := s.q.GetAutomationReceiptByIdempotency(r.Context(), generated.GetAutomationReceiptByIdempotencyParams{
		OrgID: project.OrgID, ProjectID: project.ID, IdempotencyKey: normalized.IdempotencyKey,
	})
	if getErr != nil {
		if errors.Is(getErr, pgx.ErrNoRows) {
			writeErr(w, http.StatusConflict, "idempotency key is already in use")
			return
		}
		slogError(w, "get automation receipt idempotency", getErr)
		return
	}
	if !automationReceiptMatches(existing, normalized, actor) {
		writeErr(w, http.StatusConflict, "idempotency key does not match the existing receipt")
		return
	}
	writeJSON(w, http.StatusOK, toAutomationReceiptResponse(existing))
}

func (s *Server) handleListAutomationReceipts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListAutomationReceiptsByProject(r.Context(), generated.ListAutomationReceiptsByProjectParams{
		OrgID: orgIDFrom(r.Context()), ProjectID: chi.URLParam(r, "id"), Limit: automationReceiptLimit(r),
	})
	if err != nil {
		slogError(w, "list automation receipts", err)
		return
	}
	out := make([]automationReceiptResponse, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			out = append(out, toAutomationReceiptResponse(row))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetAutomationVerification exposes a fixed 30-day, payload-free count
// of external pass claims awaiting trusted outcome verification. It is scoped
// by both project and organization and cannot be used as execution authority.
func (s *Server) handleGetAutomationVerification(w http.ResponseWriter, r *http.Request) {
	const windowHours = 30 * 24
	orgID, projectID := orgIDFrom(r.Context()), chi.URLParam(r, "id")
	if _, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: projectID, OrgScope: orgID}); err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	stats, err := s.q.GetAutomationVerificationStats(r.Context(), generated.GetAutomationVerificationStatsParams{
		OrgID: orgID, ProjectID: projectID,
		Since: pgtype.Timestamptz{Time: time.Now().UTC().Add(-windowHours * time.Hour), Valid: true},
	})
	if err != nil {
		slogError(w, "get automation verification backlog", err)
		return
	}
	state := "clear"
	if stats.PendingCount > 0 {
		state = "pending"
	}
	writeJSON(w, http.StatusOK, automationVerificationResponse{
		State: state, WindowHours: windowHours, PendingCount: stats.PendingCount,
		CICount: stats.CiCount, MergeCount: stats.MergeCount,
		DeployCount: stats.DeployCount, SyntheticCount: stats.SyntheticCount,
	})
}

// handleGetAutomationNext returns the next eligible coding-loop stage for one
// issue. A worker can poll this projection after a human approves review and
// then post its bounded CI/merge/deploy/synthetic receipt. The endpoint is
// deliberately read-only: it grants no repository, deployment, or probe
// capability and returns no telemetry or source content.
func (s *Server) handleGetAutomationNext(w http.ResponseWriter, r *http.Request) {
	issueID := strings.TrimSpace(r.URL.Query().Get("issue_id"))
	if !boundedAutomationReference(issueID, maxAutomationReceiptIssueID, true) {
		writeErr(w, http.StatusBadRequest, "issue_id is required and must be bounded")
		return
	}
	rows, err := s.q.ListAutomationReceiptsByIssue(r.Context(), generated.ListAutomationReceiptsByIssueParams{
		OrgID: orgIDFrom(r.Context()), ProjectID: chi.URLParam(r, "id"), IssueID: issueID,
		Limit: maxAutomationNextReceiptRows,
	})
	if err != nil {
		slogError(w, "get automation next stage", err)
		return
	}
	writeJSON(w, http.StatusOK, automationNextAction(issueID, rows))
}

// automationNextAction computes a bounded state-machine handoff from rows
// ordered oldest-first by the query. Only a target revision's valid terminal
// predecessor can make the next stage available; pending work is surfaced as
// state=pending and failed/blocked work is retryable. Empty or malformed rows
// never grant an action.
func automationNextAction(issueID string, rows []*generated.AutomationReceipt) automationNextResponse {
	out := automationNextResponse{IssueID: issueID, State: "not_started"}
	latestRevision := ""
	for _, row := range rows {
		if row == nil || !automationWorkflowStage(row.Stage) || row.TargetRevision == "" {
			continue
		}
		latestRevision = row.TargetRevision
	}
	if latestRevision == "" {
		return out
	}
	out.TargetRevision = latestRevision

	latest := make(map[string]*generated.AutomationReceipt, len(automationWorkflowStages))
	completed := make(map[string]*generated.AutomationReceipt, len(automationWorkflowStages))
	for _, row := range rows {
		if row == nil || row.TargetRevision != latestRevision || !automationWorkflowStage(row.Stage) {
			continue
		}
		latest[row.Stage] = row
		if automationReceiptStageComplete(row) {
			completed[row.Stage] = row
		}
	}

	for index, stage := range automationWorkflowStages {
		row := latest[stage]
		if completed[stage] != nil {
			continue
		}
		if row != nil && row.Status == "pending" {
			out.Stage = stage
			out.State = "pending"
			return out
		}
		if index > 0 {
			prerequisite := completed[automationWorkflowStages[index-1]]
			if prerequisite == nil {
				out.State = "blocked"
				return out
			}
			out.PrerequisiteReceiptID = prerequisite.ID
		}
		if row != nil && (row.Status == "failed" || row.Status == "blocked") {
			out.Available = true
			out.Stage = stage
			out.State = "retry"
			return out
		}
		if row != nil && row.Status == "passed" && automationReceiptNeedsExternalVerification(row.Stage) && row.VerificationStatus != "verified" {
			out.Stage = stage
			out.State = "verification_pending"
			return out
		}
		if row == nil {
			if index == 0 {
				out.State = "not_started"
				return out
			}
			out.Available = true
			out.Stage = stage
			out.State = "ready"
			return out
		}
		// A malformed terminal row is not an eligible action. Keep the worker
		// from treating an unverified legacy receipt as a green predecessor.
		out.Stage = stage
		out.State = "blocked"
		return out
	}
	out.State = "complete"
	return out
}

func automationWorkflowStage(stage string) bool {
	for _, candidate := range automationWorkflowStages {
		if candidate == stage {
			return true
		}
	}
	return false
}

func automationReceiptStageComplete(row *generated.AutomationReceipt) bool {
	if row == nil {
		return false
	}
	switch row.Stage {
	case "patch":
		return row.Status == "passed" && row.TargetRevision != "" && validAIEvidenceDigest(row.EvidenceRef)
	case "review":
		return row.Status == "approved" && row.TargetRevision != "" && row.ActorUserID.Valid
	case "ci", "merge", "deploy":
		return row.Status == "passed" && row.VerificationStatus == "verified" && row.TargetRevision != "" && row.RunRef != ""
	case "synthetic":
		return row.Status == "passed" && row.VerificationStatus == "verified" && row.TargetRevision != "" && row.RunRef != "" && row.EvidenceRef != ""
	default:
		return false
	}
}

func automationReceiptNeedsExternalVerification(stage string) bool {
	return stage == "ci" || stage == "merge" || stage == "deploy" || stage == "synthetic"
}

func automationReceiptLimit(r *http.Request) int32 {
	limit := defaultAutomationReceiptLimit
	if v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); err == nil && v > 0 {
		if v > maxAutomationReceiptLimit {
			v = maxAutomationReceiptLimit
		}
		limit = v
	}
	return int32(limit)
}

func normalizeAutomationReceiptRequest(req createAutomationReceiptRequest) (createAutomationReceiptRequest, error) {
	req.IssueID = strings.TrimSpace(req.IssueID)
	if !boundedAutomationReference(req.IssueID, maxAutomationReceiptIssueID, true) {
		return req, errors.New("issue_id is required and must be bounded")
	}
	req.Stage = strings.TrimSpace(req.Stage)
	if _, ok := automationReceiptStages[req.Stage]; !ok {
		return req, errors.New("stage is invalid")
	}
	req.Status = strings.TrimSpace(req.Status)
	if _, ok := automationReceiptStatuses[req.Status]; !ok {
		return req, errors.New("status is invalid")
	}
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if !validAutomationIdempotencyKey(req.IdempotencyKey) {
		return req, errors.New("idempotency_key is required and must be a safe bounded token")
	}
	req.ClaimID = strings.TrimSpace(req.ClaimID)
	if req.ClaimID != "" && !validAutomationClaimID(req.ClaimID) {
		return req, errors.New("claim_id must be a safe bounded token")
	}
	if req.ClaimID != "" && !automationClaimStage(req.Stage) {
		return req, errors.New("claim_id is only valid for executable workflow stages")
	}
	for label, value := range map[string]string{
		"evidence_ref": req.EvidenceRef, "source_revision": req.SourceRevision,
		"target_revision": req.TargetRevision, "run_ref": req.RunRef,
	} {
		if !boundedAutomationReference(value, maxAutomationReceiptReference, false) {
			return req, errors.New(label + " is too long or contains control characters")
		}
	}
	req.EvidenceRef = strings.TrimSpace(req.EvidenceRef)
	req.SourceRevision = strings.TrimSpace(req.SourceRevision)
	req.TargetRevision = strings.TrimSpace(req.TargetRevision)
	req.RunRef = strings.TrimSpace(req.RunRef)
	req.ErrorCode = strings.TrimSpace(req.ErrorCode)
	if req.Status == "failed" || req.Status == "blocked" {
		if !validAutomationErrorCode(req.ErrorCode) {
			return req, errors.New("failed or blocked receipts require a safe error_code")
		}
	} else if req.ErrorCode != "" || req.Retryable {
		return req, errors.New("error_code and retryable are only valid for failed or blocked receipts")
	}
	if req.Status == "approved" && req.Stage == "review" && req.TargetRevision == "" {
		return req, errors.New("approved review receipts require target_revision")
	}
	if req.Stage == "rollback" && (req.SourceRevision == "" || req.TargetRevision == "") {
		return req, errors.New("rollback receipts require source_revision and target_revision")
	}
	if req.Status == "passed" {
		switch req.Stage {
		case "patch", "review", "ci", "merge", "deploy", "rollback", "synthetic":
			if req.TargetRevision == "" {
				return req, errors.New("passed workflow receipts require target_revision")
			}
		}
		if req.Stage == "patch" && !validAIEvidenceDigest(req.EvidenceRef) {
			return req, errors.New("passed patch receipts require a 64-character lowercase AI evidence digest in evidence_ref")
		}
		switch req.Stage {
		case "ci", "merge", "deploy", "rollback", "synthetic":
			if req.RunRef == "" {
				return req, errors.New("passed execution receipts require run_ref")
			}
		}
		// A synthetic pass is the final acceptance gate. Keep a bounded
		// reference to the observed check result (for example, a canary run or
		// probe report) so a runner id alone cannot masquerade as verification.
		// The reference remains opaque and is never dereferenced by Flare.
		if req.Stage == "synthetic" && req.EvidenceRef == "" {
			return req, errors.New("passed synthetic receipts require evidence_ref")
		}
		if automationClaimStage(req.Stage) && req.ClaimID == "" {
			return req, errors.New("passed execution receipts require claim_id")
		}
	}
	return req, nil
}

func validAIEvidenceDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func automationReceiptPrerequisite(req createAutomationReceiptRequest) (struct {
	stage  string
	status string
}, bool) {
	key := req.Stage + ":" + req.Status
	prerequisite, ok := automationReceiptPrerequisites[key]
	return prerequisite, ok
}

func boundedAutomationReference(value string, max int, required bool) bool {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return false
	}
	if len(value) > max {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validAutomationIdempotencyKey(value string) bool {
	if value == "" || len(value) > maxAutomationIdempotencyKey {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:-", r) {
			continue
		}
		return false
	}
	return true
}

func validAutomationErrorCode(value string) bool {
	if value == "" || len(value) > maxAutomationReceiptErrorCode {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}

func automationReceiptTerminal(status string) bool {
	return status == "passed" || status == "failed" || status == "blocked"
}

func automationReceiptMatches(row *generated.AutomationReceipt, req createAutomationReceiptRequest, actor pgtype.Text) bool {
	if row == nil {
		return false
	}
	if row.ActorUserID.Valid != actor.Valid || (row.ActorUserID.Valid && row.ActorUserID.String != actor.String) {
		return false
	}
	return row.IssueID == req.IssueID && row.Stage == req.Stage && row.Status == req.Status &&
		row.IdempotencyKey == req.IdempotencyKey && row.EvidenceRef == req.EvidenceRef &&
		row.SourceRevision == req.SourceRevision && row.TargetRevision == req.TargetRevision &&
		row.RunRef == req.RunRef && row.Retryable == req.Retryable && row.ErrorCode == req.ErrorCode &&
		automationReceiptClaimID(row) == req.ClaimID
}

func automationClaimStage(stage string) bool {
	_, ok := automationClaimStages[stage]
	return ok
}

func automationReceiptClaimID(row *generated.AutomationReceipt) string {
	if row == nil || !row.ClaimID.Valid {
		return ""
	}
	return row.ClaimID.String
}

func toAutomationReceiptResponse(row *generated.AutomationReceipt) automationReceiptResponse {
	out := automationReceiptResponse{
		ID: row.ID, IssueID: row.IssueID, Stage: row.Stage, Status: row.Status,
		VerificationStatus: row.VerificationStatus,
		IdempotencyKey:     row.IdempotencyKey, EvidenceRef: row.EvidenceRef,
		SourceRevision: row.SourceRevision, TargetRevision: row.TargetRevision,
		RunRef: row.RunRef, Retryable: row.Retryable, ErrorCode: row.ErrorCode,
		CreatedAt: receiptTimestamp(row.CreatedAt), UpdatedAt: receiptTimestamp(row.UpdatedAt),
		CompletedAt: receiptTimestamp(row.CompletedAt),
	}
	if row.ActorUserID.Valid {
		out.ActorUserID = row.ActorUserID.String
	}
	return out
}

func toInsertedAutomationReceiptResponse(row *generated.InsertAutomationReceiptRow) automationReceiptResponse {
	return toAutomationReceiptResponse(&generated.AutomationReceipt{
		ID: row.ID, OrgID: row.OrgID, ProjectID: row.ProjectID, IssueID: row.IssueID,
		Stage: row.Stage, Status: row.Status, VerificationStatus: row.VerificationStatus,
		IdempotencyKey: row.IdempotencyKey, EvidenceRef: row.EvidenceRef,
		SourceRevision: row.SourceRevision, TargetRevision: row.TargetRevision,
		RunRef: row.RunRef, ActorUserID: row.ActorUserID, Retryable: row.Retryable,
		ErrorCode: row.ErrorCode, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		CompletedAt: row.CompletedAt, ClaimID: row.ClaimID,
	})
}
