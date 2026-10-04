package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/stagereceipt"
)

var stageRevision = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var stageDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var stageProfileRevision = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type stageReceiptVerifier interface {
	Verify(context.Context, string, string, string, string, stagereceipt.Claim) (stagereceipt.Attestation, error)
}

type stageBindingRequest struct {
	StageTenantID     string `json:"stage_tenant_id"`
	StageCredentialID string `json:"stage_credential_id"`
	Repository        string `json:"repository"`
	ProgramID         string `json:"program_id"`
	CIPipeline        string `json:"ci_pipeline"`
	DeployPipeline    string `json:"deploy_pipeline"`
	Credential        string `json:"credential"`
}

type stageBinding struct {
	OrgID, ProjectID, StageTenantID, StageCredentialID, Repository, ProgramID string
	CIPipeline, MergePipeline, DeployPipeline, CredentialCiphertext, Epoch    string
}

type stageBindingResponse struct {
	StageTenantID        string `json:"stage_tenant_id"`
	StageCredentialID    string `json:"stage_credential_id"`
	Repository           string `json:"repository"`
	ProgramID            string `json:"program_id"`
	CIPipeline           string `json:"ci_pipeline"`
	MergePipeline        string `json:"merge_pipeline"`
	DeployPipeline       string `json:"deploy_pipeline"`
	CredentialConfigured bool   `json:"credential_configured"`
	VerifierConfigured   bool   `json:"verifier_configured"`
}

func (b stageBinding) response(ready bool) stageBindingResponse {
	return stageBindingResponse{
		StageTenantID: b.StageTenantID, StageCredentialID: b.StageCredentialID, Repository: b.Repository, ProgramID: b.ProgramID,
		CIPipeline: b.CIPipeline, MergePipeline: b.MergePipeline, DeployPipeline: b.DeployPipeline,
		CredentialConfigured: b.CredentialCiphertext != "", VerifierConfigured: ready,
	}
}

func stageBindingField(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if c < 33 || c == 127 {
			return false
		}
	}
	return true
}

func validStageBinding(req stageBindingRequest) bool {
	return stageBindingField(req.StageTenantID, 128) && stageBindingField(req.StageCredentialID, 256) && stageBindingField(req.Repository, 256) &&
		stageBindingField(req.ProgramID, 128) && stageBindingField(req.CIPipeline, 128) &&
		stageBindingField(req.DeployPipeline, 128) &&
		stageBindingField(req.Credential, 4096) && !strings.Contains(req.Credential, " ")
}

func (s *Server) getStageBinding(ctx context.Context, orgID, projectID string) (stageBinding, error) {
	var b stageBinding
	err := s.pool.QueryRow(ctx, `
		SELECT b.org_id, b.project_id, b.stage_tenant_id, b.stage_credential_id, b.repository, b.program_id,
		       b.ci_pipeline, b.merge_pipeline, b.deploy_pipeline, b.credential_ciphertext, b.binding_epoch
		FROM automation_stage_bindings b JOIN projects p ON p.id = b.project_id AND p.org_id = b.org_id
		WHERE b.org_id = $1 AND b.project_id = $2`, orgID, projectID).Scan(
		&b.OrgID, &b.ProjectID, &b.StageTenantID, &b.StageCredentialID, &b.Repository, &b.ProgramID,
		&b.CIPipeline, &b.MergePipeline, &b.DeployPipeline, &b.CredentialCiphertext, &b.Epoch)
	return b, err
}

func (s *Server) handleGetStageVerifierBinding(w http.ResponseWriter, r *http.Request) {
	b, err := s.getStageBinding(r.Context(), orgIDFrom(r.Context()), chi.URLParam(r, "id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "Stage verifier binding not found")
		return
	}
	if err != nil {
		slogError(w, "get Stage verifier binding", err)
		return
	}
	writeJSON(w, http.StatusOK, b.response(stagereceipt.ValidateEndpoint(s.cfg.StageVerifyURL) == nil && stagereceipt.ValidatePeerIP(s.cfg.StageVerifyPeerIP) == nil))
}

// A project administrator binds one Stage tenant/repository/program to the
// project. Every PUT requires a new credential; it can never move a stored
// bearer token to a new identity or destination by omitting the secret.
func (s *Server) handleSetStageVerifierBinding(w http.ResponseWriter, r *http.Request) {
	var req stageBindingRequest
	if decodeJSON(r, &req) != nil || !validStageBinding(req) {
		writeErr(w, http.StatusBadRequest, "invalid Stage verifier binding")
		return
	}
	if s.secrets == nil || !s.secrets.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "encrypted credential storage is unavailable")
		return
	}
	if stagereceipt.ValidateEndpoint(s.cfg.StageVerifyURL) != nil || stagereceipt.ValidatePeerIP(s.cfg.StageVerifyPeerIP) != nil {
		writeErr(w, http.StatusServiceUnavailable, "Stage verifier endpoint is not configured")
		return
	}
	ciphertext := s.secrets.Encrypt(req.Credential)
	if ciphertext == req.Credential || !strings.HasPrefix(ciphertext, "enc:v1:") {
		writeErr(w, http.StatusServiceUnavailable, "encrypted credential storage failed")
		return
	}
	projectID, orgID := chi.URLParam(r, "id"), orgIDFrom(r.Context())
	if req.StageCredentialID != "flare:"+projectID && !strings.HasPrefix(req.StageCredentialID, "flare:"+projectID+":") {
		writeErr(w, http.StatusBadRequest, "Stage credential is not bound to this project")
		return
	}
	if strings.HasPrefix(req.StageCredentialID, "flare:"+projectID+":") {
		suffix := strings.TrimPrefix(req.StageCredentialID, "flare:"+projectID+":")
		if !stageProfileRevision.MatchString(suffix) {
			writeErr(w, http.StatusBadRequest, "invalid Stage credential revision")
			return
		}
	}
	var b stageBinding
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO automation_stage_bindings
			(project_id, org_id, stage_tenant_id, stage_credential_id, repository, program_id, ci_pipeline,
			 merge_pipeline, deploy_pipeline, credential_ciphertext, binding_epoch)
		SELECT p.id, p.org_id, $3, $4, $5, $6, $7, 'controlled-merge', $8, $9, $10
		FROM projects p WHERE p.id = $1 AND p.org_id = $2
		ON CONFLICT (project_id) DO UPDATE SET
			stage_tenant_id = EXCLUDED.stage_tenant_id, repository = EXCLUDED.repository,
			stage_credential_id = EXCLUDED.stage_credential_id,
			program_id = EXCLUDED.program_id, ci_pipeline = EXCLUDED.ci_pipeline,
			merge_pipeline = EXCLUDED.merge_pipeline, deploy_pipeline = EXCLUDED.deploy_pipeline,
			credential_ciphertext = EXCLUDED.credential_ciphertext,
			binding_epoch = EXCLUDED.binding_epoch, updated_at = now()
		WHERE automation_stage_bindings.org_id = EXCLUDED.org_id
		RETURNING org_id, project_id, stage_tenant_id, stage_credential_id, repository, program_id,
		          ci_pipeline, merge_pipeline, deploy_pipeline, credential_ciphertext, binding_epoch`,
		projectID, orgID, req.StageTenantID, req.StageCredentialID, req.Repository, req.ProgramID,
		req.CIPipeline, req.DeployPipeline, ciphertext, id.New()).Scan(
		&b.OrgID, &b.ProjectID, &b.StageTenantID, &b.StageCredentialID, &b.Repository, &b.ProgramID,
		&b.CIPipeline, &b.MergePipeline, &b.DeployPipeline, &b.CredentialCiphertext, &b.Epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		slogError(w, "set Stage verifier binding", err)
		return
	}
	s.audit(r.Context(), "automation.stage-verifier.configure", projectID)
	writeJSON(w, http.StatusOK, b.response(true))
}

func (s *Server) handleDeleteStageVerifierBinding(w http.ResponseWriter, r *http.Request) {
	projectID, orgID := chi.URLParam(r, "id"), orgIDFrom(r.Context())
	cmd, err := s.pool.Exec(r.Context(),
		`DELETE FROM automation_stage_bindings WHERE org_id = $1 AND project_id = $2`, orgID, projectID)
	if err != nil {
		slogError(w, "delete Stage verifier binding", err)
		return
	}
	if cmd.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "Stage verifier binding not found")
		return
	}
	s.audit(r.Context(), "automation.stage-verifier.delete", projectID)
	w.WriteHeader(http.StatusNoContent)
}

type stageReceiptSnapshot struct {
	ID, IssueID, Stage, Status, VerificationStatus, RunRef, EvidenceRef, SourceRevision, TargetRevision string
}

func getStageReceipt(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID, projectID, receiptID string) (stageReceiptSnapshot, error) {
	var row stageReceiptSnapshot
	err := q.QueryRow(ctx, `SELECT id, issue_id, stage, status, verification_status, run_ref,
		       evidence_ref, source_revision, target_revision
		FROM automation_receipts WHERE id = $1 AND org_id = $2 AND project_id = $3`,
		receiptID, orgID, projectID).Scan(&row.ID, &row.IssueID, &row.Stage,
		&row.Status, &row.VerificationStatus, &row.RunRef, &row.EvidenceRef,
		&row.SourceRevision, &row.TargetRevision)
	return row, err
}

func stageClaimForReceipt(projectID string, b stageBinding, row stageReceiptSnapshot) (stagereceipt.Claim, bool) {
	claim := stagereceipt.Claim{
		ProjectID: projectID, Kind: row.Stage, Repository: b.Repository, ProgramID: b.ProgramID,
		RunRef: row.RunRef, EvidenceRef: row.EvidenceRef,
		SourceRevision: row.SourceRevision, TargetRevision: row.TargetRevision,
	}
	if row.Status != "passed" || row.VerificationStatus != "unverified" ||
		!stageRevision.MatchString(row.TargetRevision) || row.RunRef == "" {
		return claim, false
	}
	switch row.Stage {
	case "ci":
		claim.Pipeline = b.CIPipeline
		return claim, row.SourceRevision == "" && row.EvidenceRef == ""
	case "merge":
		claim.Pipeline = b.MergePipeline
		return claim, stageRevision.MatchString(row.SourceRevision) && strings.HasPrefix(row.RunRef, "merge:")
	case "deploy":
		claim.Pipeline = b.DeployPipeline
		return claim, row.SourceRevision == row.TargetRevision && stageDigest.MatchString(row.EvidenceRef)
	case "synthetic":
		// The external probe is a later output of the exact deploy-flare run.
		// Stage must bind its digest to the same immutable image and cutover.
		claim.Pipeline = b.DeployPipeline
		return claim, row.SourceRevision == row.TargetRevision && stageDigest.MatchString(row.EvidenceRef)
	default:
		return claim, false
	}
}

type stageVerificationResponse struct {
	ReceiptID          string    `json:"receipt_id"`
	VerificationStatus string    `json:"verification_status"`
	AttestationID      string    `json:"attestation_id"`
	VerifiedAt         time.Time `json:"verified_at"`
}

func (s *Server) handleVerifyAutomationReceipt(w http.ResponseWriter, r *http.Request) {
	projectID, receiptID, orgID := chi.URLParam(r, "id"), chi.URLParam(r, "receiptID"), orgIDFrom(r.Context())
	if s.stageVerifyLimiter != nil && !s.stageVerifyLimiter.Allow("org:"+orgID) {
		writeErr(w, http.StatusTooManyRequests, "Stage verification rate limit exceeded")
		return
	}
	if stagereceipt.ValidateEndpoint(s.cfg.StageVerifyURL) != nil || stagereceipt.ValidatePeerIP(s.cfg.StageVerifyPeerIP) != nil || s.stageVerifier == nil ||
		s.secrets == nil || !s.secrets.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "Stage verification is not configured")
		return
	}
	b, err := s.getStageBinding(r.Context(), orgID, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "Stage verifier binding not found")
		return
	}
	if err != nil {
		slogError(w, "get Stage verifier binding", err)
		return
	}
	if !strings.HasPrefix(b.CredentialCiphertext, "enc:v1:") {
		writeErr(w, http.StatusServiceUnavailable, "Stage credential is not encrypted")
		return
	}
	credential, err := s.secrets.Decrypt(b.CredentialCiphertext)
	if err != nil || credential == "" {
		writeErr(w, http.StatusServiceUnavailable, "Stage credential cannot be opened")
		return
	}
	row, err := getStageReceipt(r.Context(), s.pool, orgID, projectID, receiptID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "receipt not found")
		return
	}
	if err != nil {
		slogError(w, "get Stage receipt", err)
		return
	}
	claim, ok := stageClaimForReceipt(projectID, b, row)
	if !ok {
		writeErr(w, http.StatusConflict, "receipt has no verifiable Stage claim")
		return
	}
	proof, err := s.stageVerifier.Verify(r.Context(), s.cfg.StageVerifyURL, credential, b.StageTenantID, b.StageCredentialID, claim)
	if errors.Is(err, stagereceipt.ErrNoProof) {
		writeErr(w, http.StatusConflict, "Stage has no matching durable proof")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "Stage verification is unavailable")
		return
	}
	if err := s.promoteStageReceipt(r.Context(), b, row, proof); errors.Is(err, errStageReceiptChanged) {
		writeErr(w, http.StatusConflict, "binding or receipt changed during verification")
		return
	} else if err != nil {
		slogError(w, "persist Stage receipt proof", err)
		return
	}
	writeJSON(w, http.StatusOK, stageVerificationResponse{
		ReceiptID: row.ID, VerificationStatus: "verified", AttestationID: proof.AttestationID,
		VerifiedAt: proof.VerifiedAt,
	})
}

var errStageReceiptChanged = errors.New("Stage binding or receipt changed")

// promoteStageReceipt atomically pins the binding epoch and receipt fields,
// inserts Stage's immutable proof, flips the advisory status, and writes an
// audit record. A concurrent token rotation, deletion, receipt change, or
// second verifier loses the compare-and-swap and cannot mint a green row.
func (s *Server) promoteStageReceipt(ctx context.Context, b stageBinding, row stageReceiptSnapshot, proof stagereceipt.Attestation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var epoch string
	if err := tx.QueryRow(ctx, `SELECT binding_epoch FROM automation_stage_bindings
		WHERE org_id = $1 AND project_id = $2 FOR UPDATE`, b.OrgID, b.ProjectID).Scan(&epoch); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errStageReceiptChanged
		}
		return err
	}
	if epoch != b.Epoch {
		return errStageReceiptChanged
	}
	var current stageReceiptSnapshot
	err = tx.QueryRow(ctx, `SELECT id, issue_id, stage, status, verification_status, run_ref,
		       evidence_ref, source_revision, target_revision FROM automation_receipts
		WHERE id = $1 AND org_id = $2 AND project_id = $3 FOR UPDATE`,
		row.ID, b.OrgID, b.ProjectID).Scan(&current.ID, &current.IssueID, &current.Stage,
		&current.Status, &current.VerificationStatus, &current.RunRef, &current.EvidenceRef,
		&current.SourceRevision, &current.TargetRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return errStageReceiptChanged
	}
	if err != nil {
		return err
	}
	if current != row {
		return errStageReceiptChanged
	}
	if !proof.Verified || proof.AttestationID == "" || proof.VerifiedAt.IsZero() ||
		proof.ProjectID != b.ProjectID || proof.TenantID != b.StageTenantID || proof.CredentialID != b.StageCredentialID ||
		proof.Repository != b.Repository || proof.ProgramID != b.ProgramID ||
		proof.Pipeline != stagePipelineForKind(b, row.Stage) ||
		proof.Kind != row.Stage || proof.RunRef != row.RunRef || proof.EvidenceRef != row.EvidenceRef ||
		proof.SourceRevision != row.SourceRevision || proof.TargetRevision != row.TargetRevision {
		return errStageReceiptChanged
	}
	if _, err := tx.Exec(ctx, `INSERT INTO automation_receipt_attestations
		(receipt_id, org_id, project_id, issue_id, stage_tenant_id, stage_credential_id, attestation_id,
		 repository, program_id, pipeline, kind, run_ref, evidence_ref, source_revision,
		 target_revision, image_ref, image_id, stage_verified_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		row.ID, b.OrgID, b.ProjectID, row.IssueID, proof.TenantID, proof.CredentialID, proof.AttestationID,
		proof.Repository, proof.ProgramID, proof.Pipeline, proof.Kind, proof.RunRef,
		proof.EvidenceRef, proof.SourceRevision, proof.TargetRevision, proof.ImageRef,
		proof.ImageID, proof.VerifiedAt); err != nil {
		return err
	}
	cmd, err := tx.Exec(ctx, `UPDATE automation_receipts SET verification_status = 'verified', updated_at = now()
		WHERE id = $1 AND org_id = $2 AND project_id = $3 AND verification_status = 'unverified'`,
		row.ID, b.OrgID, b.ProjectID)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() != 1 {
		return errStageReceiptChanged
	}
	var actor pgtype.Text
	// The actor is set by the caller below through context. API key verification
	// has a null actor, matching the existing audit convention.
	if uid := userIDFrom(ctx); uid != "" {
		actor = pgText(uid)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (id, org_id, actor_user_id, action, target)
		VALUES ($1,$2,$3,$4,$5)`, id.New(), b.OrgID, actor,
		"automation.receipt.verify", fmt.Sprintf("%s:%s", row.ID, proof.AttestationID)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func stagePipelineForKind(b stageBinding, kind string) string {
	switch kind {
	case "ci":
		return b.CIPipeline
	case "merge":
		return b.MergePipeline
	case "deploy":
		return b.DeployPipeline
	default:
		return ""
	}
}
