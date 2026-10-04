package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/secretbox"
	"github.com/bright-interaction/flare/internal/stagereceipt"
)

type stageVerifierFunc func(context.Context, string, string, string, string, stagereceipt.Claim) (stagereceipt.Attestation, error)

func (f stageVerifierFunc) Verify(ctx context.Context, url, token, tenant, credential string, claim stagereceipt.Claim) (stagereceipt.Attestation, error) {
	return f(ctx, url, token, tenant, credential, claim)
}

const stageTestSHA = "0123456789abcdef0123456789abcdef01234567"

func stageHandlerRequest(method, projectID, receiptID, orgID, body string) *http.Request {
	path := "/api/projects/" + projectID + "/stage-verifier"
	if receiptID != "" {
		path = "/api/projects/" + projectID + "/automation-receipts/" + receiptID + "/verify"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", projectID)
	if receiptID != "" {
		route.URLParams.Add("receiptID", receiptID)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	return req.WithContext(context.WithValue(ctx, ctxOrgID, orgID))
}

func stageBindingJSON(projectID string) string {
	data, _ := json.Marshal(stageBindingRequest{
		StageTenantID: "tenant-1", StageCredentialID: "flare:" + projectID + ":v2",
		Repository: "repo-1", ProgramID: "flare", CIPipeline: "ci-flare", DeployPipeline: "deploy-flare",
		Credential: "scoped-stage-token",
	})
	return string(data)
}

func TestStageBindingAndReceiptPromotionTransaction(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	q := generated.New(pool)
	ctx := context.Background()
	issue, err := q.UpsertIssue(ctx, generated.UpsertIssueParams{
		ID: id.New(), ProjectID: project.ID, OrgID: project.OrgID, Fingerprint: id.New(),
		Title: "Stage verify", Culprit: "test", Level: "error", Platform: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := q.CreateUser(ctx, generated.CreateUserParams{
		ID: id.New(), OrgID: project.OrgID, Email: id.New() + "@example.test", PasswordHash: "hash", Role: "member",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedAutomationPatchFixture(t, pool, project.OrgID, project.ID, issue.ID, stageTestSHA, "patch-before-stage-ci")
	if _, err = q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Stage: "review", Status: "approved", IdempotencyKey: "review-stage-test", TargetRevision: stageTestSHA,
		ActorUserID: pgtype.Text{String: actor.ID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Stage: "ci", TargetRevision: stageTestSHA, WorkerRef: "worker-1",
		LeasedUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Stage: "ci", Status: "passed", IdempotencyKey: "ci-stage-test", ClaimID: claim.ID,
		RunRef: "run-1", TargetRevision: stageTestSHA,
		CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil || receipt.VerificationStatus != "unverified" {
		t.Fatalf("advisory receipt = %#v, %v", receipt, err)
	}
	s := serverForDB(pool)
	s.secrets = secretbox.New("a dedicated test secret")
	s.cfg.StageVerifyURL = "https://stage.example.test/attestations/verify"
	put := httptest.NewRecorder()
	s.handleSetStageVerifierBinding(put, stageHandlerRequest(http.MethodPut, project.ID, "", project.OrgID, stageBindingJSON(project.ID)))
	if put.Code != http.StatusOK || strings.Contains(put.Body.String(), "scoped-stage-token") {
		t.Fatalf("bind = %d: %s", put.Code, put.Body.String())
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT credential_ciphertext FROM automation_stage_bindings WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).Scan(&stored); err != nil || !strings.HasPrefix(stored, "enc:v1:") {
		t.Fatalf("credential storage = %q, %v", stored, err)
	}
	foreign := httptest.NewRecorder()
	s.handleGetStageVerifierBinding(foreign, stageHandlerRequest(http.MethodGet, project.ID, "", "other-org", ""))
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant binding = %d", foreign.Code)
	}
	s.stageVerifier = stageVerifierFunc(func(context.Context, string, string, string, string, stagereceipt.Claim) (stagereceipt.Attestation, error) {
		return stagereceipt.Attestation{}, stagereceipt.ErrUnavailable
	})
	unavailable := httptest.NewRecorder()
	s.handleVerifyAutomationReceipt(unavailable, stageHandlerRequest(http.MethodPost, project.ID, receipt.ID, project.OrgID, ""))
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("Stage outage promoted advisory receipt: %d", unavailable.Code)
	}
	oldBinding, err := s.getStageBinding(ctx, project.OrgID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldReceipt, err := getStageReceipt(ctx, pool, project.OrgID, project.ID, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotate := httptest.NewRecorder()
	s.handleSetStageVerifierBinding(rotate, stageHandlerRequest(http.MethodPut, project.ID, "", project.OrgID, stageBindingJSON(project.ID)))
	if rotate.Code != http.StatusOK {
		t.Fatalf("rotate binding = %d: %s", rotate.Code, rotate.Body.String())
	}
	newBinding, err := s.getStageBinding(ctx, project.OrgID, project.ID)
	if err != nil || newBinding.Epoch == oldBinding.Epoch {
		t.Fatalf("binding epoch did not rotate: %v", err)
	}
	if err := s.promoteStageReceipt(ctx, oldBinding, oldReceipt, stagereceipt.Attestation{}); !errors.Is(err, errStageReceiptChanged) {
		t.Fatalf("stale binding promoted receipt: %v", err)
	}
	s.stageVerifier = stageVerifierFunc(func(_ context.Context, url, token, tenant, credential string, c stagereceipt.Claim) (stagereceipt.Attestation, error) {
		if url != s.cfg.StageVerifyURL || token != "scoped-stage-token" || tenant != "tenant-1" || credential != "flare:"+project.ID+":v2" ||
			c.ProjectID != project.ID || c.Repository != "repo-1" || c.ProgramID != "flare" || c.Pipeline != "ci-flare" ||
			c.Kind != "ci" || c.RunRef != "run-1" || c.SourceRevision != "" || c.TargetRevision != stageTestSHA {
			t.Fatalf("wrong server-owned Stage claim: %#v, %q/%q/%q", c, tenant, credential, url)
		}
		return stagereceipt.Attestation{Verified: true, AttestationID: "stgatt_" + strings.Repeat("a", 64),
			ProjectID: c.ProjectID, TenantID: tenant, CredentialID: credential, Repository: c.Repository,
			ProgramID: c.ProgramID, Pipeline: c.Pipeline, Kind: c.Kind, RunRef: c.RunRef,
			EvidenceRef: c.EvidenceRef, SourceRevision: c.SourceRevision, TargetRevision: c.TargetRevision,
			VerifiedAt: time.Now().UTC()}, nil
	})
	foreignReceipt := httptest.NewRecorder()
	s.handleVerifyAutomationReceipt(foreignReceipt, stageHandlerRequest(http.MethodPost, project.ID, receipt.ID, "other-org", ""))
	if foreignReceipt.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant verification = %d", foreignReceipt.Code)
	}
	ok := httptest.NewRecorder()
	s.handleVerifyAutomationReceipt(ok, stageHandlerRequest(http.MethodPost, project.ID, receipt.ID, project.OrgID, ""))
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), `"verification_status":"verified"`) {
		t.Fatalf("Stage promotion = %d: %s", ok.Code, ok.Body.String())
	}
	var status, proofID, credentialID string
	if err := pool.QueryRow(ctx, `SELECT r.verification_status,a.attestation_id,a.stage_credential_id
		FROM automation_receipts r JOIN automation_receipt_attestations a ON a.receipt_id=r.id
		WHERE r.id=$1 AND r.org_id=$2 AND r.project_id=$3`, receipt.ID, project.OrgID, project.ID).Scan(&status, &proofID, &credentialID); err != nil || status != "verified" || proofID != "stgatt_"+strings.Repeat("a", 64) || credentialID != "flare:"+project.ID+":v2" {
		t.Fatalf("proof/status = %q %q %q %v", status, proofID, credentialID, err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE org_id=$1 AND action='automation.receipt.verify' AND target=$2`, project.OrgID, receipt.ID+":"+proofID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit = %d %v", auditCount, err)
	}
}

func TestStageClaimRequiresStageFields(t *testing.T) {
	b := stageBinding{CIPipeline: "ci-flare", MergePipeline: "controlled-merge", DeployPipeline: "deploy-flare", Repository: "repo", ProgramID: "flare"}
	for _, tc := range []struct {
		stage, source, target, evidence, run string
		want                                 bool
	}{
		{"ci", "", stageTestSHA, "", "run", true},
		{"ci", stageTestSHA, stageTestSHA, "", "run", false},
		{"merge", stageTestSHA, stageTestSHA, "", "merge:branch", true},
		{"merge", "", stageTestSHA, "", "merge:branch", false},
		{"deploy", stageTestSHA, stageTestSHA, strings.Repeat("a", 64), "run", true},
		{"deploy", "", stageTestSHA, strings.Repeat("a", 64), "run", false},
		{"synthetic", stageTestSHA, stageTestSHA, strings.Repeat("b", 64), "deploy-run", true},
		{"synthetic", stageTestSHA, stageTestSHA, "probe", "run", false},
		{"synthetic", "", stageTestSHA, strings.Repeat("b", 64), "deploy-run", false},
	} {
		_, ok := stageClaimForReceipt("p", b, stageReceiptSnapshot{Stage: tc.stage, Status: "passed", VerificationStatus: "unverified", SourceRevision: tc.source, TargetRevision: tc.target, EvidenceRef: tc.evidence, RunRef: tc.run})
		if ok != tc.want {
			t.Errorf("%s source %q verifiable=%v want %v", tc.stage, tc.source, ok, tc.want)
		}
	}
}
