package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func TestAutomationPassedReceiptOnePerLiveClaimReturnsConflict(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	q := generated.New(pool)
	ctx := context.Background()
	issue, err := q.UpsertIssue(ctx, generated.UpsertIssueParams{
		ID: id.New(), ProjectID: project.ID, OrgID: project.OrgID,
		Fingerprint: id.New(), Title: "CI claim", Culprit: "test", Level: "error", Platform: "go",
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
	seedAutomationPatchFixture(t, pool, project.OrgID, project.ID, issue.ID, "rev-1", "patch-before-ci-claim")
	if _, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Stage: "review", Status: "approved", IdempotencyKey: "review-claim",
		TargetRevision: "rev-1", ActorUserID: pgtype.Text{String: actor.ID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Stage: "ci", TargetRevision: "rev-1", WorkerRef: "worker-1",
		LeasedUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	submit := func(key string) *httptest.ResponseRecorder {
		body, err := json.Marshal(createAutomationReceiptRequest{
			IssueID: issue.ID, Stage: "ci", Status: "passed", IdempotencyKey: key,
			ClaimID: claim.ID, TargetRevision: "rev-1", RunRef: "opaque-run",
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/projects/"+project.ID+"/automation-receipts", strings.NewReader(string(body)))
		route := chi.NewRouteContext()
		route.URLParams.Add("id", project.ID)
		rctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
		req = req.WithContext(context.WithValue(rctx, ctxOrgID, project.OrgID))
		out := httptest.NewRecorder()
		s.handleCreateAutomationReceipt(out, req)
		return out
	}
	first := submit("ci-claim-first")
	if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), `"verification_status":"unverified"`) {
		t.Fatalf("first advisory pass = %d: %s", first.Code, first.Body.String())
	}
	second := submit("ci-claim-second")
	if second.Code != http.StatusConflict {
		t.Fatalf("second pass under live claim = %d: %s", second.Code, second.Body.String())
	}
}

func TestAutomationMergeClaimMovesSourceRevisionToDeployRevision(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	q := generated.New(pool)
	ctx := context.Background()
	const sourceRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const mergeRevision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	issue, err := q.UpsertIssue(ctx, generated.UpsertIssueParams{
		ID: id.New(), ProjectID: project.ID, OrgID: project.OrgID,
		Fingerprint: id.New(), Title: "Merge revision", Culprit: "test", Level: "error", Platform: "go",
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
	seedAutomationPatchFixture(t, pool, project.OrgID, project.ID, issue.ID, sourceRevision, "patch-before-merge-transition")
	if _, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Stage: "review", Status: "approved", IdempotencyKey: "review-merge-revision",
		TargetRevision: sourceRevision, ActorUserID: pgtype.Text{String: actor.ID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	lease := pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}
	claim := func(stage, revision string) (*generated.AutomationClaim, error) {
		return q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
			ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
			Stage: stage, TargetRevision: revision, WorkerRef: "test-worker", LeasedUntil: lease,
		})
	}
	ciClaim, err := claim("ci", sourceRevision)
	if err != nil {
		t.Fatal(err)
	}
	ci, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Stage: "ci", Status: "passed", IdempotencyKey: "ci-merge-revision",
		TargetRevision: sourceRevision, RunRef: "ci-run", ClaimID: ciClaim.ID,
		CompletedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The SQL transition test starts after a trusted verifier has established
	// the CI proof. The verifier itself has separate exact-proof tests.
	if _, err := pool.Exec(ctx, `UPDATE automation_receipts SET verification_status = 'verified' WHERE id = $1`, ci.ID); err != nil {
		t.Fatal(err)
	}
	mergeClaim, err := claim("merge", sourceRevision)
	if err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	body, err := json.Marshal(createAutomationReceiptRequest{
		IssueID: issue.ID, Stage: "merge", Status: "passed", IdempotencyKey: "merge-source-to-result",
		ClaimID: mergeClaim.ID, SourceRevision: sourceRevision, TargetRevision: mergeRevision, RunRef: "merge:branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+project.ID+"/automation-receipts", strings.NewReader(string(body)))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", project.ID)
	rctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	req = req.WithContext(context.WithValue(rctx, ctxOrgID, project.OrgID))
	out := httptest.NewRecorder()
	s.handleCreateAutomationReceipt(out, req)
	if out.Code != http.StatusCreated {
		t.Fatalf("merge receipt = %d: %s", out.Code, out.Body.String())
	}
	var merged automationReceiptResponse
	if err := json.Unmarshal(out.Body.Bytes(), &merged); err != nil {
		t.Fatal(err)
	}
	if merged.SourceRevision != sourceRevision || merged.TargetRevision != mergeRevision || merged.VerificationStatus != "unverified" {
		t.Fatalf("merge receipt lost revision transition: %#v", merged)
	}
	if _, err := pool.Exec(ctx, `UPDATE automation_receipts SET verification_status = 'verified' WHERE id = $1`, merged.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := claim("merge", sourceRevision); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("verified merge source revision remained claimable: %v", err)
	}
	if _, err := claim("deploy", sourceRevision); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unmerged source revision became deployable: %v", err)
	}
	deployClaim, err := claim("deploy", mergeRevision)
	if err != nil {
		t.Fatalf("verified merge result revision was not deployable: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE automation_claims SET leased_until = now() - interval '1 second' WHERE id = $1`, deployClaim.ID); err != nil {
		t.Fatal(err)
	}
	seedAutomationPatchFixture(t, pool, project.OrgID, project.ID, issue.ID,
		"cccccccccccccccccccccccccccccccccccccccc", "newer-patch-generation")
	if _, err := claim("deploy", mergeRevision); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old merge result remained deployable after a newer patch: %v", err)
	}
	previousMerge, err := q.HasAutomationReceiptEvidence(ctx, generated.HasAutomationReceiptEvidenceParams{
		OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Stage: "merge", Status: "passed", TargetRevision: mergeRevision,
	})
	if err != nil || previousMerge {
		t.Fatalf("old merge satisfied current patch prerequisite: evidence=%v, err=%v", previousMerge, err)
	}
}

func seedAutomationPatchFixture(t *testing.T, pool *pgxpool.Pool, orgID, projectID, issueID, revision, key string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO automation_receipts (id, org_id, project_id, issue_id, stage, status,
			idempotency_key, evidence_ref, target_revision, completed_at)
		VALUES ($1, $2, $3, $4, 'patch', 'passed', $5, $6, $7, now())`,
		id.New(), orgID, projectID, issueID, key,
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", revision)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAutomationVerificationRequiresScopedProject(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	request := func(orgID, projectID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/automation-verification", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", projectID)
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
		req = req.WithContext(context.WithValue(ctx, ctxOrgID, orgID))
		out := httptest.NewRecorder()
		s.handleGetAutomationVerification(out, req)
		return out
	}
	if got := request("other-org", project.ID); got.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant project returned %d: %s", got.Code, got.Body.String())
	}
	if got := request(project.OrgID, "missing-project"); got.Code != http.StatusNotFound {
		t.Fatalf("missing project returned %d: %s", got.Code, got.Body.String())
	}
	if got := request(project.OrgID, project.ID); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"state":"clear"`) {
		t.Fatalf("owned project verification summary = %d: %s", got.Code, got.Body.String())
	}
}

func TestAutomationReceiptLimitIsBounded(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int32
	}{
		{"", defaultAutomationReceiptLimit},
		{"?limit=25", 25},
		{"?limit=5000", maxAutomationReceiptLimit},
		{"?limit=0", defaultAutomationReceiptLimit},
		{"?limit=bad", defaultAutomationReceiptLimit},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/projects/p/automation-receipts"+tc.query, nil)
			if got := automationReceiptLimit(r); got != tc.want {
				t.Fatalf("automationReceiptLimit(%q) = %d, want %d", tc.query, got, tc.want)
			}
		})
	}
}

func TestNormalizeAutomationReceiptRequestStrict(t *testing.T) {
	forged := httptest.NewRequest(http.MethodPost, "/api/projects/p/automation-receipts", strings.NewReader(`{"issue_id":"i","stage":"ci","status":"passed","idempotency_key":"k","verification_status":"verified"}`))
	var forgedRequest createAutomationReceiptRequest
	if err := decodeJSON(forged, &forgedRequest); err == nil {
		t.Fatal("client-controlled verification_status was accepted")
	}
	valid := createAutomationReceiptRequest{
		IssueID: "issue-1", Stage: "ci", Status: "passed", IdempotencyKey: "run-1", ClaimID: "claim-1",
		SourceRevision: "abc123", TargetRevision: "def456", RunRef: "ci-17",
	}
	if got, err := normalizeAutomationReceiptRequest(valid); err != nil || got.Stage != "ci" {
		t.Fatalf("valid request rejected: %#v, %v", got, err)
	}
	for name, req := range map[string]createAutomationReceiptRequest{
		"unknown stage":                       {IssueID: "i", Stage: "execute", Status: "pending", IdempotencyKey: "k"},
		"unknown status":                      {IssueID: "i", Stage: "ci", Status: "running", IdempotencyKey: "k"},
		"missing key":                         {IssueID: "i", Stage: "ci", Status: "pending"},
		"unsafe key":                          {IssueID: "i", Stage: "ci", Status: "pending", IdempotencyKey: "k/1"},
		"failed no code":                      {IssueID: "i", Stage: "ci", Status: "failed", IdempotencyKey: "k"},
		"passed error":                        {IssueID: "i", Stage: "ci", Status: "passed", IdempotencyKey: "k", ErrorCode: "oops"},
		"pending retry":                       {IssueID: "i", Stage: "ci", Status: "pending", IdempotencyKey: "k", Retryable: true},
		"control ref":                         {IssueID: "i", Stage: "ci", Status: "pending", IdempotencyKey: "k", RunRef: "run\n1"},
		"oversized evidence":                  {IssueID: "i", Stage: "ci", Status: "pending", IdempotencyKey: "k", EvidenceRef: strings.Repeat("x", maxAutomationReceiptReference+1)},
		"review approval without revision":    {IssueID: "i", Stage: "review", Status: "approved", IdempotencyKey: "k"},
		"patch pass without revision":         {IssueID: "i", Stage: "patch", Status: "passed", IdempotencyKey: "k"},
		"patch pass without AI evidence":      {IssueID: "i", Stage: "patch", Status: "passed", IdempotencyKey: "k", TargetRevision: "rev-1"},
		"patch pass with invalid AI evidence": {IssueID: "i", Stage: "patch", Status: "passed", IdempotencyKey: "k", TargetRevision: "rev-1", EvidenceRef: "not-a-digest"},
		"ci pass without run":                 {IssueID: "i", Stage: "ci", Status: "passed", IdempotencyKey: "k", TargetRevision: "rev-1"},
		"merge pass without source":           {IssueID: "i", Stage: "merge", Status: "passed", IdempotencyKey: "k", TargetRevision: "rev-2", RunRef: "merge-1", ClaimID: "claim-1"},
		"synthetic pass without evidence":     {IssueID: "i", Stage: "synthetic", Status: "passed", IdempotencyKey: "k", TargetRevision: "rev-1", RunRef: "synthetic-17"},
		"rollback missing source":             {IssueID: "i", Stage: "rollback", Status: "pending", IdempotencyKey: "k", TargetRevision: "rev-1"},
		"rollback missing target":             {IssueID: "i", Stage: "rollback", Status: "pending", IdempotencyKey: "k", SourceRevision: "rev-2"},
		"rollback pass without run":           {IssueID: "i", Stage: "rollback", Status: "passed", IdempotencyKey: "k", SourceRevision: "rev-2", TargetRevision: "rev-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeAutomationReceiptRequest(req); err == nil {
				t.Fatal("request unexpectedly accepted")
			}
		})
	}
}

func TestNormalizeAutomationReceiptRequestAcceptsOnlyLowercaseAIEvidenceForPatch(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := createAutomationReceiptRequest{
		IssueID: "issue-1", Stage: "patch", Status: "passed", IdempotencyKey: "patch-1",
		TargetRevision: "rev-1", EvidenceRef: digest, ClaimID: "claim-1",
	}
	if _, err := normalizeAutomationReceiptRequest(req); err != nil {
		t.Fatalf("patch receipt with AI evidence rejected: %v", err)
	}
	if validAIEvidenceDigest(strings.ToUpper(digest)) {
		t.Fatal("uppercase AI evidence digest accepted")
	}
}

func TestNormalizeAutomationReceiptRequestAcceptsBoundedSyntheticEvidence(t *testing.T) {
	req := createAutomationReceiptRequest{
		IssueID: "issue-1", Stage: "synthetic", Status: "passed", IdempotencyKey: "synthetic-1",
		TargetRevision: "rev-1", RunRef: "probe-run-17", EvidenceRef: "canary:checkout:2026-10-02T12:00Z", ClaimID: "claim-1",
	}
	if _, err := normalizeAutomationReceiptRequest(req); err != nil {
		t.Fatalf("synthetic receipt with evidence rejected: %v", err)
	}
}

func TestAutomationReceiptPrerequisitesFollowTheHumanApprovedRevision(t *testing.T) {
	for _, tc := range []struct {
		stage, status, wantStage, wantStatus string
		want                                 bool
	}{
		{stage: "patch", status: "passed", want: false},
		{stage: "review", status: "approved", wantStage: "patch", wantStatus: "passed", want: true},
		{stage: "ci", status: "passed", wantStage: "review", wantStatus: "approved", want: true},
		{stage: "merge", status: "passed", wantStage: "ci", wantStatus: "passed", want: true},
		{stage: "deploy", status: "passed", wantStage: "merge", wantStatus: "passed", want: true},
		{stage: "synthetic", status: "passed", wantStage: "deploy", wantStatus: "passed", want: true},
		{stage: "rollback", status: "passed", want: false},
		{stage: "ci", status: "failed", want: false},
	} {
		t.Run(tc.stage+":"+tc.status, func(t *testing.T) {
			got, ok := automationReceiptPrerequisite(createAutomationReceiptRequest{Stage: tc.stage, Status: tc.status})
			if ok != tc.want {
				t.Fatalf("prerequisite presence = %v, want %v", ok, tc.want)
			}
			if ok && (got.stage != tc.wantStage || got.status != tc.wantStatus) {
				t.Fatalf("prerequisite = %#v, want %s:%s", got, tc.wantStage, tc.wantStatus)
			}
		})
	}
}

func automationNextRow(id, stage, status, revision string) *generated.AutomationReceipt {
	row := &generated.AutomationReceipt{ID: id, IssueID: "issue-1", Stage: stage, Status: status, TargetRevision: revision}
	if stage == "patch" && status == "passed" {
		row.EvidenceRef = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}
	if stage == "review" && status == "approved" {
		row.ActorUserID = pgtype.Text{String: "human-1", Valid: true}
	}
	if stage == "ci" || stage == "merge" || stage == "deploy" || stage == "synthetic" {
		row.RunRef = id + "-run"
		if stage == "merge" {
			row.SourceRevision = revision
		}
		if status == "passed" {
			// This fixture represents a receipt promoted by a future trusted verifier.
			row.VerificationStatus = "verified"
		}
	}
	if stage == "synthetic" && status == "passed" {
		row.EvidenceRef = id + "-evidence"
	}
	return row
}

func TestAutomationNextActionIsTenantSafeStateMachine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rows      []*generated.AutomationReceipt
		available bool
		state     string
		stage     string
		prereqID  string
		revision  string
	}{
		{name: "not started", state: "not_started"},
		{
			name: "review follows passed patch", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
			}, available: true, state: "ready", stage: "review", prereqID: "patch-1",
		},
		{
			name: "ci follows human review", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
			}, available: true, state: "ready", stage: "ci", prereqID: "review-1",
		},
		{
			name: "pending ci is not executable", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
				automationNextRow("ci-1", "ci", "pending", "rev-1"),
			}, state: "pending", stage: "ci",
		},
		{
			name: "caller reported ci pass awaits verification", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
				{ID: "ci-advisory", Stage: "ci", Status: "passed", TargetRevision: "rev-1", RunRef: "opaque-run"},
			}, state: "verification_pending", stage: "ci", prereqID: "review-1",
		},
		{
			name: "newer unverified ci does not mask verified ci", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
				automationNextRow("ci-verified", "ci", "passed", "rev-1"),
				{ID: "ci-advisory", Stage: "ci", Status: "passed", TargetRevision: "rev-1", RunRef: "opaque-run"},
			}, available: true, state: "ready", stage: "merge", prereqID: "ci-verified",
		},
		{
			name: "older unverified ci does not mask verified ci", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
				{ID: "ci-advisory", Stage: "ci", Status: "passed", TargetRevision: "rev-1", RunRef: "opaque-run"},
				automationNextRow("ci-verified", "ci", "passed", "rev-1"),
			}, available: true, state: "ready", stage: "merge", prereqID: "ci-verified",
		},
		{
			name: "review without human actor blocks ci", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				{ID: "review-1", IssueID: "issue-1", Stage: "review", Status: "approved", TargetRevision: "rev-1"},
			}, state: "blocked", stage: "review", prereqID: "patch-1",
		},
		{
			name: "patch without AI evidence blocks review", rows: []*generated.AutomationReceipt{
				{ID: "patch-1", IssueID: "issue-1", Stage: "patch", Status: "passed", TargetRevision: "rev-1"},
			}, state: "blocked", stage: "patch",
		},
		{
			name: "failed ci can retry", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
				{ID: "ci-1", IssueID: "issue-1", Stage: "ci", Status: "failed", TargetRevision: "rev-1"},
			}, available: true, state: "retry", stage: "ci", prereqID: "review-1",
		},
		{
			name: "synthetic follows deploy", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
				automationNextRow("ci-1", "ci", "passed", "rev-1"),
				automationNextRow("merge-1", "merge", "passed", "rev-1"),
				automationNextRow("deploy-1", "deploy", "passed", "rev-1"),
			}, available: true, state: "ready", stage: "synthetic", prereqID: "deploy-1",
		},
		{
			name: "complete after evidence", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
				automationNextRow("ci-1", "ci", "passed", "rev-1"),
				automationNextRow("merge-1", "merge", "passed", "rev-1"),
				automationNextRow("deploy-1", "deploy", "passed", "rev-1"),
				automationNextRow("synthetic-1", "synthetic", "passed", "rev-1"),
			}, state: "complete",
		},
		{
			name: "synthetic without evidence blocks completion", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-1", "review", "approved", "rev-1"),
				automationNextRow("ci-1", "ci", "passed", "rev-1"),
				automationNextRow("merge-1", "merge", "passed", "rev-1"),
				automationNextRow("deploy-1", "deploy", "passed", "rev-1"),
				{ID: "synthetic-1", IssueID: "issue-1", Stage: "synthetic", Status: "passed", VerificationStatus: "verified", TargetRevision: "rev-1", RunRef: "probe-1"},
			}, state: "blocked", stage: "synthetic", prereqID: "deploy-1",
		},
		{
			name: "wrong revision cannot satisfy", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-2", "review", "approved", "rev-2"),
			}, available: true, state: "ready", stage: "review", prereqID: "patch-1",
		},
		{
			name: "verified merge moves deploy to result revision", rows: func() []*generated.AutomationReceipt {
				merge := automationNextRow("merge-1", "merge", "passed", "rev-2")
				merge.SourceRevision = "rev-1"
				return []*generated.AutomationReceipt{
					automationNextRow("patch-1", "patch", "passed", "rev-1"),
					automationNextRow("review-1", "review", "approved", "rev-1"),
					automationNextRow("ci-1", "ci", "passed", "rev-1"), merge,
				}
			}(), available: true, state: "ready", stage: "deploy", prereqID: "merge-1", revision: "rev-2",
		},
		{
			name: "unverified merge cannot move revision", rows: func() []*generated.AutomationReceipt {
				merge := automationNextRow("merge-1", "merge", "passed", "rev-2")
				merge.SourceRevision = "rev-1"
				merge.VerificationStatus = "unverified"
				return []*generated.AutomationReceipt{
					automationNextRow("patch-1", "patch", "passed", "rev-1"),
					automationNextRow("review-1", "review", "approved", "rev-1"),
					automationNextRow("ci-1", "ci", "passed", "rev-1"), merge,
				}
			}(), state: "verification_pending", stage: "merge", prereqID: "ci-1",
		},
		{
			name: "wrong merge source cannot move revision", rows: func() []*generated.AutomationReceipt {
				merge := automationNextRow("merge-1", "merge", "passed", "rev-2")
				merge.SourceRevision = "other-branch"
				return []*generated.AutomationReceipt{
					automationNextRow("patch-1", "patch", "passed", "rev-1"),
					automationNextRow("review-1", "review", "approved", "rev-1"),
					automationNextRow("ci-1", "ci", "passed", "rev-1"), merge,
				}
			}(), available: true, state: "ready", stage: "merge", prereqID: "ci-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := automationNextAction("issue-1", tc.rows)
			if got.Available != tc.available || got.State != tc.state || got.Stage != tc.stage || got.PrerequisiteReceiptID != tc.prereqID {
				t.Fatalf("next action = %#v, want available=%v state=%q stage=%q prerequisite=%q", got, tc.available, tc.state, tc.stage, tc.prereqID)
			}
			if got.IssueID != "issue-1" || (got.TargetRevision != "" && got.TargetRevision != "rev-1" && got.TargetRevision != "rev-2") {
				t.Fatalf("next action provenance = %#v", got)
			}
			if tc.revision != "" && got.TargetRevision != tc.revision {
				t.Fatalf("next target revision = %q, want %q", got.TargetRevision, tc.revision)
			}
		})
	}
}

func TestAutomationReceiptResponseIsPayloadFree(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	row := &generated.AutomationReceipt{
		ID: "receipt-1", OrgID: "org-secret", ProjectID: "project-secret", IssueID: "issue-1",
		Stage: "review", Status: "approved", IdempotencyKey: "review-1",
		EvidenceRef: "evidence-1", SourceRevision: "abc", TargetRevision: "def", RunRef: "run-1",
		ActorUserID: pgtype.Text{String: "user-1", Valid: true},
		CreatedAt:   pgtype.Timestamptz{Time: now, Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
	body, err := json.Marshal(toAutomationReceiptResponse(row))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	text := string(body)
	for _, forbidden := range []string{"org-secret", "project-secret", "prompt", "completion", "credential", "secret-value", "source contents"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, text)
		}
	}
	for _, want := range []string{`"id":"receipt-1"`, `"stage":"review"`, `"status":"approved"`, `"actor_user_id":"user-1"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q: %s", want, text)
		}
	}
}

func TestAutomationReceiptRouteAuthAndNoExecution(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	body := string(src)
	readStart := strings.Index(body, "r.Use(s.requireAuth)")
	memberStart := strings.Index(body, `s.requireRole("member")`)
	if readStart < 0 || memberStart < 0 || readStart >= memberStart {
		t.Fatal("router auth/read groups changed")
	}
	readBlock := body[readStart:memberStart]
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/automation-receipts", s.handleListAutomationReceipts)`) {
		t.Fatal("automation receipt GET is not in authenticated viewer read group")
	}
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/automation-next", s.handleGetAutomationNext)`) {
		t.Fatal("automation next GET is not in authenticated viewer read group")
	}
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/automation-verification", s.handleGetAutomationVerification)`) {
		t.Fatal("automation verification GET is not in authenticated viewer read group")
	}
	memberEnd := strings.Index(body[memberStart:], `s.requireRole("admin")`)
	if memberEnd < 0 {
		t.Fatal("member route group has no admin boundary")
	}
	memberBlock := body[memberStart : memberStart+memberEnd]
	if !strings.Contains(memberBlock, `r.Post("/projects/{id}/automation-receipts", s.handleCreateAutomationReceipt)`) {
		t.Fatal("automation receipt POST is not in member route group")
	}
	if !strings.Contains(memberBlock, `r.Post("/projects/{id}/automation-claims", s.handleCreateAutomationClaim)`) {
		t.Fatal("automation claim POST is not in member route group")
	}
	if strings.Contains(memberBlock, "handleDeploy") || strings.Contains(memberBlock, "exec.Command") {
		t.Fatal("automation receipt route group must not execute or deploy work")
	}
}
