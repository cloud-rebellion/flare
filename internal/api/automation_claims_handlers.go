package api

import (
	"errors"
	"net/http"
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
	defaultAutomationClaimLeaseSeconds = 10 * 60
	maxAutomationClaimLeaseSeconds     = 60 * 60
	maxAutomationClaimWorkerRef        = 128
	maxAutomationClaimID               = 128
)

var automationClaimStages = map[string]struct{}{
	"patch": {}, "ci": {}, "merge": {}, "deploy": {}, "synthetic": {},
}

type createAutomationClaimRequest struct {
	IssueID        string    `json:"issue_id"`
	Stage          string    `json:"stage"`
	TargetRevision string    `json:"target_revision"`
	WorkerRef      string    `json:"worker_ref"`
	EvidenceRef    string    `json:"evidence_ref"`
	LeaseSeconds   int       `json:"lease_seconds"`
	LeasedUntil    time.Time `json:"-"`
}

// automationClaimResponse is the bounded handoff to an external worker. The
// claim id is an opaque, short-lived capability; Flare never treats it as a
// repository token or dereferences any caller supplied reference.
type automationClaimResponse struct {
	ID             string    `json:"id"`
	IssueID        string    `json:"issue_id"`
	Stage          string    `json:"stage"`
	TargetRevision string    `json:"target_revision"`
	WorkerRef      string    `json:"worker_ref"`
	EvidenceRef    string    `json:"evidence_ref,omitempty"`
	LeasedUntil    time.Time `json:"leased_until"`
}

// handleCreateAutomationClaim atomically leases one eligible workflow stage.
// Polling automation-next remains useful for discovery, but workers must claim
// before running CI, merge, deploy, or synthetic work so two workers cannot
// execute the same revision concurrently. The route only records a lease.
func (s *Server) handleCreateAutomationClaim(w http.ResponseWriter, r *http.Request) {
	var req createAutomationClaimRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	normalized, err := normalizeAutomationClaimRequest(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	projectID := chi.URLParam(r, "id")
	orgID := orgIDFrom(r.Context())
	project, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: projectID, OrgScope: orgID})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	issue, err := s.q.GetIssue(r.Context(), generated.GetIssueParams{ID: normalized.IssueID, OrgID: orgID})
	if err != nil || issue == nil || issue.ProjectID != project.ID {
		writeErr(w, http.StatusNotFound, "issue not found")
		return
	}

	row, err := s.q.ClaimAutomationStage(r.Context(), generated.ClaimAutomationStageParams{
		ID:             id.New(),
		OrgID:          project.OrgID,
		ProjectID:      project.ID,
		IssueID:        normalized.IssueID,
		Stage:          normalized.Stage,
		TargetRevision: normalized.TargetRevision,
		WorkerRef:      normalized.WorkerRef,
		EvidenceRef:    normalized.EvidenceRef,
		LeasedUntil:    pgtype.Timestamptz{Time: normalized.LeasedUntil, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Keep the response deliberately generic: an active claim, a missing
			// predecessor, and an already-completed stage are all non-claimable
			// without revealing workflow rows or another worker's identity.
			writeErr(w, http.StatusConflict, "workflow stage is not claimable")
			return
		}
		slogError(w, "claim automation stage", err)
		return
	}
	s.audit(r.Context(), "automation.claim", row.ID)
	writeJSON(w, http.StatusCreated, toAutomationClaimResponse(row))
}

func normalizeAutomationClaimRequest(req createAutomationClaimRequest) (createAutomationClaimRequest, error) {
	req.IssueID = strings.TrimSpace(req.IssueID)
	if !boundedAutomationReference(req.IssueID, maxAutomationReceiptIssueID, true) {
		return req, errors.New("issue_id is required and must be bounded")
	}
	req.Stage = strings.TrimSpace(req.Stage)
	if _, ok := automationClaimStages[req.Stage]; !ok {
		return req, errors.New("stage is not claimable")
	}
	for label, value := range map[string]string{
		"target_revision": req.TargetRevision,
		"worker_ref":      req.WorkerRef,
		"evidence_ref":    req.EvidenceRef,
	} {
		required := label != "evidence_ref" || req.Stage == "patch"
		if !boundedAutomationReference(value, maxAutomationReferenceForClaim(label), required) {
			return req, errors.New(label + " is required and must be bounded")
		}
	}
	req.TargetRevision = strings.TrimSpace(req.TargetRevision)
	req.WorkerRef = strings.TrimSpace(req.WorkerRef)
	req.EvidenceRef = strings.TrimSpace(req.EvidenceRef)
	if req.Stage == "patch" {
		if !validAIEvidenceDigest(req.EvidenceRef) {
			return req, errors.New("patch claims require a 64-character lowercase AI evidence digest")
		}
	} else if req.EvidenceRef != "" {
		return req, errors.New("evidence_ref is only valid for patch claims")
	}
	if req.LeaseSeconds == 0 {
		req.LeaseSeconds = defaultAutomationClaimLeaseSeconds
	}
	if req.LeaseSeconds < 1 || req.LeaseSeconds > maxAutomationClaimLeaseSeconds {
		return req, errors.New("lease_seconds must be between 1 and 3600")
	}
	req.LeasedUntil = time.Now().UTC().Add(time.Duration(req.LeaseSeconds) * time.Second)
	return req, nil
}

func maxAutomationReferenceForClaim(label string) int {
	if label == "worker_ref" {
		return maxAutomationClaimWorkerRef
	}
	return maxAutomationReceiptReference
}

func toAutomationClaimResponse(row *generated.AutomationClaim) automationClaimResponse {
	return automationClaimResponse{
		ID: row.ID, IssueID: row.IssueID, Stage: row.Stage,
		TargetRevision: row.TargetRevision, WorkerRef: row.WorkerRef,
		EvidenceRef: row.EvidenceRef, LeasedUntil: row.LeasedUntil.Time.UTC(),
	}
}

// Keep claim ids safe when callers include one in a receipt. The database
// performs the authoritative tenant/stage/revision/lease check.
func validAutomationClaimID(value string) bool {
	if value == "" || len(value) > maxAutomationClaimID {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
