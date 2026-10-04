package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func TestAutomationReceiptScopeAndIdempotency(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	otherOrg, otherProject := seedProject(t, ctx, q)
	issue := seedIssue(t, ctx, q, org, project, "coding-loop", "error")
	actor, err := q.CreateUser(ctx, generated.CreateUserParams{
		ID: id.New(), OrgID: org, Email: id.New() + "@example.test", PasswordHash: "hash", Role: "member",
	})
	if err != nil {
		t.Fatalf("create workflow actor: %v", err)
	}
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	leaseUntil := pgtype.Timestamptz{Time: time.Now().UTC().Add(10 * time.Minute), Valid: true}
	const evidenceDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	aiReceipt, err := q.InsertAIActionReceipt(ctx, generated.InsertAIActionReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Operation: "triage", Status: "started", Provider: "openai", StartedAt: now,
		Retryable: true, EvidenceDigest: evidenceDigest,
	})
	if err != nil {
		t.Fatalf("insert AI evidence receipt: %v", err)
	}
	if _, err := q.CompleteAIActionReceipt(ctx, generated.CompleteAIActionReceiptParams{
		ID: aiReceipt.ID, OrgID: org, ProjectID: project, Status: "succeeded",
		Provider: "openai", CompletedAt: now, Retryable: false, EvidenceDigest: evidenceDigest,
	}); err != nil {
		t.Fatalf("complete AI evidence receipt: %v", err)
	}
	patchClaim, err := q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "patch", TargetRevision: "def", WorkerRef: "worker-1",
		EvidenceRef: evidenceDigest, LeasedUntil: leaseUntil,
	})
	if err != nil || patchClaim == nil {
		t.Fatalf("claim patch stage: %v", err)
	}
	_, err = q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "patch", TargetRevision: "def", WorkerRef: "worker-2",
		EvidenceRef: evidenceDigest, LeasedUntil: leaseUntil,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate active patch claim error = %v, want pgx.ErrNoRows", err)
	}
	patch, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "patch", Status: "passed", IdempotencyKey: "patch-1",
		SourceRevision: "abc", TargetRevision: "def", RunRef: "patch-17", EvidenceRef: evidenceDigest, CompletedAt: now, ClaimID: patchClaim.ID,
	})
	if err != nil || patch == nil {
		t.Fatalf("insert patch receipt: %v", err)
	}
	_, err = q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "patch", TargetRevision: "def", WorkerRef: "worker-2",
		EvidenceRef: evidenceDigest, LeasedUntil: leaseUntil,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("completed patch claim error = %v, want pgx.ErrNoRows", err)
	}
	review, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "review", Status: "approved", IdempotencyKey: "review-1",
		TargetRevision: "def", ActorUserID: pgtype.Text{String: actor.ID, Valid: true},
	})
	if err != nil || review == nil {
		t.Fatalf("insert review receipt: %v", err)
	}
	ciClaim, err := q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "ci", TargetRevision: "def", WorkerRef: "worker-1", LeasedUntil: leaseUntil,
	})
	if err != nil || ciClaim == nil {
		t.Fatalf("claim ci stage: %v", err)
	}
	params := generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "ci", Status: "passed", IdempotencyKey: "run-1",
		SourceRevision: "abc", TargetRevision: "def", RunRef: "ci-17",
		CompletedAt: now, ClaimID: ciClaim.ID,
	}
	row, err := q.InsertAutomationReceipt(ctx, params)
	if err != nil {
		t.Fatalf("insert automation receipt: %v", err)
	}
	if row.Stage != "ci" || row.Status != "passed" || row.IssueID != issue {
		t.Fatalf("inserted receipt = %#v", row)
	}
	if row.VerificationStatus != "unverified" {
		t.Fatalf("caller-supplied CI outcome status = %q, want unverified", row.VerificationStatus)
	}
	if allowed, checkErr := q.HasAutomationReceiptEvidence(ctx, generated.HasAutomationReceiptEvidenceParams{
		OrgID: org, ProjectID: project, IssueID: issue, Stage: "ci", Status: "passed", TargetRevision: "def",
	}); checkErr != nil || allowed {
		t.Fatalf("unverified CI as prerequisite = %v, %v; want false", allowed, checkErr)
	}
	if _, claimErr := q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "merge", TargetRevision: "def", WorkerRef: "worker-1", LeasedUntil: leaseUntil,
	}); !errors.Is(claimErr, pgx.ErrNoRows) {
		t.Fatalf("merge claim after unverified CI = %v, want no rows", claimErr)
	}
	secondKey := params
	secondKey.ID = id.New()
	secondKey.IdempotencyKey = "run-2"
	if _, dupErr := q.InsertAutomationReceipt(ctx, secondKey); !errors.Is(dupErr, pgx.ErrNoRows) {
		t.Fatalf("second pass under same live claim = %v, want no rows", dupErr)
	}
	rollback, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "rollback", Status: "passed", IdempotencyKey: "rollback-1",
		SourceRevision: "def", TargetRevision: "abc", RunRef: "rollback-17", CompletedAt: now,
	})
	if err != nil || rollback == nil {
		t.Fatalf("insert rollback receipt: %v", err)
	}

	// A retry with the same key is ignored by the unique constraint. The API
	// fetches the existing row and compares the immutable request fields before
	// returning it as an idempotent 200.
	_, err = q.InsertAutomationReceipt(ctx, params)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate insert error = %v, want pgx.ErrNoRows", err)
	}
	got, err := q.GetAutomationReceiptByIdempotency(ctx, generated.GetAutomationReceiptByIdempotencyParams{
		OrgID: org, ProjectID: project, IdempotencyKey: "run-1",
	})
	if err != nil || got.ID != row.ID {
		t.Fatalf("idempotency lookup = %#v, %v", got, err)
	}

	rows, err := q.ListAutomationReceiptsByProject(ctx, generated.ListAutomationReceiptsByProjectParams{
		OrgID: org, ProjectID: project, Limit: 10,
	})
	if err != nil || len(rows) != 4 {
		t.Fatalf("scoped list = %v (%d rows), want patch/review/ci/rollback receipts", err, len(rows))
	}
	issueRows, err := q.ListAutomationReceiptsByIssue(ctx, generated.ListAutomationReceiptsByIssueParams{
		OrgID: org, ProjectID: project, IssueID: issue, Limit: 10,
	})
	if err != nil || len(issueRows) != 4 {
		t.Fatalf("issue-scoped list = %v (%d rows), want four receipts", err, len(issueRows))
	}
	otherIssueRows, err := q.ListAutomationReceiptsByIssue(ctx, generated.ListAutomationReceiptsByIssueParams{
		OrgID: otherOrg, ProjectID: otherProject, IssueID: issue, Limit: 10,
	})
	if err != nil {
		t.Fatalf("cross-tenant issue list: %v", err)
	}
	if len(otherIssueRows) != 0 {
		t.Fatalf("cross-tenant issue list returned %d rows", len(otherIssueRows))
	}
	crossTenant, err := q.ListAutomationReceiptsByProject(ctx, generated.ListAutomationReceiptsByProjectParams{
		OrgID: otherOrg, ProjectID: otherProject, Limit: 10,
	})
	if err != nil {
		t.Fatalf("cross-tenant list: %v", err)
	}
	if len(crossTenant) != 0 {
		t.Fatalf("cross-tenant list returned %d rows", len(crossTenant))
	}
	evidence, err := q.HasAutomationReceiptEvidence(ctx, generated.HasAutomationReceiptEvidenceParams{
		OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "review", Status: "approved", TargetRevision: "def",
	})
	if err != nil || !evidence {
		t.Fatalf("review prerequisite evidence = %v, %v; want true", evidence, err)
	}
	crossEvidence, err := q.HasAutomationReceiptEvidence(ctx, generated.HasAutomationReceiptEvidenceParams{
		OrgID: otherOrg, ProjectID: otherProject, IssueID: issue,
		Stage: "review", Status: "approved", TargetRevision: "def",
	})
	if err != nil {
		t.Fatalf("cross-tenant evidence query: %v", err)
	}
	if crossEvidence {
		t.Fatal("cross-tenant workflow evidence was visible")
	}
	wrongRevision, err := q.HasAutomationReceiptEvidence(ctx, generated.HasAutomationReceiptEvidenceParams{
		OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "review", Status: "approved", TargetRevision: "other-revision",
	})
	if err != nil {
		t.Fatalf("wrong-revision evidence query: %v", err)
	}
	if wrongRevision {
		t.Fatal("workflow evidence crossed target revisions")
	}
	_, err = q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "patch", Status: "passed", IdempotencyKey: "patch-ungrounded",
		TargetRevision: "ungrounded", EvidenceRef: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd",
		CompletedAt: now,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ungrounded patch insert error = %v, want pgx.ErrNoRows", err)
	}
	ungroundedEvidence, err := q.HasAutomationReceiptEvidence(ctx, generated.HasAutomationReceiptEvidenceParams{
		OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "patch", Status: "passed", TargetRevision: "ungrounded",
	})
	if err != nil {
		t.Fatalf("ungrounded patch evidence query: %v", err)
	}
	if ungroundedEvidence {
		t.Fatal("patch without a matching AI receipt was accepted as workflow evidence")
	}
	// The schema's approved-review check deliberately prevents deleting an
	// actor while its human approval remains retained.
	if _, err := q.DeleteUser(ctx, generated.DeleteUserParams{ID: actor.ID, OrgID: org}); err == nil {
		t.Fatal("review actor was deleted while an approved receipt still references it")
	}
	retainedActorEvidence, err := q.HasAutomationReceiptEvidence(ctx, generated.HasAutomationReceiptEvidenceParams{
		OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "review", Status: "approved", TargetRevision: "def",
	})
	if err != nil {
		t.Fatalf("retained-actor evidence query: %v", err)
	}
	if !retainedActorEvidence {
		t.Fatal("review evidence disappeared although actor deletion was rejected")
	}
	_ = pool
}

func TestSyntheticReceiptCanCarryProbeEvidence(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	issue := seedIssue(t, ctx, q, org, project, "synthetic acceptance", "error")
	actor, err := q.CreateUser(ctx, generated.CreateUserParams{
		ID: id.New(), OrgID: org, Email: id.New() + "@example.test", PasswordHash: "hash", Role: "member",
	})
	if err != nil {
		t.Fatalf("create workflow actor: %v", err)
	}
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	leaseUntil := pgtype.Timestamptz{Time: time.Now().UTC().Add(10 * time.Minute), Valid: true}
	const revision = "release-1"
	acceptanceParams := generated.GetDeployAcceptanceStatsParams{
		OrgID: org, ProjectID: project,
		Since:       pgtype.Timestamptz{Time: now.Time.Add(-time.Hour), Valid: true},
		GraceCutoff: pgtype.Timestamptz{Time: now.Time.Add(time.Minute), Valid: true},
	}

	if _, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "review", Status: "approved", IdempotencyKey: "synthetic-review",
		TargetRevision: revision, ActorUserID: pgtype.Text{String: actor.ID, Valid: true},
	}); err != nil {
		t.Fatalf("insert review receipt: %v", err)
	}
	claimStage := func(stage, key, runRef string) *generated.AutomationClaim {
		t.Helper()
		claim, claimErr := q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
			ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
			Stage: stage, TargetRevision: revision, WorkerRef: "worker-1", LeasedUntil: leaseUntil,
		})
		if claimErr != nil || claim == nil {
			t.Fatalf("claim %s stage: %v", stage, claimErr)
		}
		receipt, receiptErr := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
			ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
			Stage: stage, Status: "passed", IdempotencyKey: key,
			TargetRevision: revision, RunRef: runRef, CompletedAt: now, ClaimID: claim.ID,
		})
		if receiptErr != nil {
			t.Fatalf("insert %s receipt: %v", stage, receiptErr)
		}
		if receipt.VerificationStatus != "unverified" {
			t.Fatalf("%s outcome was preverified", stage)
		}
		if stage == "deploy" {
			count, checkErr := q.GetDeployAcceptanceStats(ctx, acceptanceParams)
			if checkErr != nil || count != 0 {
				t.Fatalf("unverified deploy accepted by watchdog = %d, %v", count, checkErr)
			}
		}
		// This test-only update represents a future trusted verifier. The
		// public API cannot promote a caller's opaque run_ref.
		if _, promoteErr := pool.Exec(ctx, "UPDATE automation_receipts SET verification_status='verified' WHERE id=$1", receipt.ID); promoteErr != nil {
			t.Fatalf("promote %s test fixture: %v", stage, promoteErr)
		}
		return claim
	}
	claimStage("ci", "synthetic-ci", "ci-run-1")
	claimStage("merge", "synthetic-merge", "merge-run-1")
	claimStage("deploy", "synthetic-deploy", "deploy-run-1")

	syntheticClaim := claimStageOnly(t, ctx, q, org, project, issue, "synthetic", revision, leaseUntil)
	synthetic, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "synthetic", Status: "passed", IdempotencyKey: "synthetic-pass",
		TargetRevision: revision, RunRef: "probe-run-1", EvidenceRef: "probe-result-1",
		CompletedAt: now, ClaimID: syntheticClaim.ID,
	})
	if err != nil || synthetic == nil {
		t.Fatalf("insert synthetic receipt with probe evidence: %v", err)
	}
	if synthetic.EvidenceRef != "probe-result-1" {
		t.Fatalf("synthetic evidence_ref = %q, want probe-result-1", synthetic.EvidenceRef)
	}
	if synthetic.VerificationStatus != "unverified" {
		t.Fatalf("synthetic evidence accepted without verifier: %q", synthetic.VerificationStatus)
	}
	count, err := q.GetDeployAcceptanceStats(ctx, acceptanceParams)
	if err != nil || count != 1 {
		t.Fatalf("unverified synthetic suppressed accepted deploy gap = %d, %v", count, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE automation_receipts SET verification_status='verified' WHERE id=$1", synthetic.ID); err != nil {
		t.Fatal(err)
	}
	count, err = q.GetDeployAcceptanceStats(ctx, acceptanceParams)
	if err != nil || count != 0 {
		t.Fatalf("verified synthetic did not close accepted deploy gap = %d, %v", count, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE automation_receipts SET evidence_ref='' WHERE id=$1", synthetic.ID); err != nil {
		t.Fatal(err)
	}
	count, err = q.GetDeployAcceptanceStats(ctx, acceptanceParams)
	if err != nil || count != 1 {
		t.Fatalf("synthetic without probe evidence closed deploy gap = %d, %v", count, err)
	}
	_ = pool
}

func claimStageOnly(t *testing.T, ctx context.Context, q *generated.Queries, org, project, issue, stage, revision string, leaseUntil pgtype.Timestamptz) *generated.AutomationClaim {
	t.Helper()
	claim, err := q.ClaimAutomationStage(ctx, generated.ClaimAutomationStageParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: stage, TargetRevision: revision, WorkerRef: "worker-1", LeasedUntil: leaseUntil,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim %s stage: %v", stage, err)
	}
	return claim
}

func TestUnverifiedCIClaimCanBeRetriedAfterLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	otherOrg, otherProject := seedProject(t, ctx, q)
	issue := seedIssue(t, ctx, q, org, project, "claim recovery", "error")
	actor, err := q.CreateUser(ctx, generated.CreateUserParams{
		ID: id.New(), OrgID: org, Email: id.New() + "@example.test", PasswordHash: "hash", Role: "member",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "review", Status: "approved", IdempotencyKey: "recover-review",
		TargetRevision: "rev-1", ActorUserID: pgtype.Text{String: actor.ID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	lease := pgtype.Timestamptz{Time: time.Now().UTC().Add(10 * time.Minute), Valid: true}
	claim := claimStageOnly(t, ctx, q, org, project, issue, "ci", "rev-1", lease)
	first, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "ci", Status: "passed", IdempotencyKey: "recover-ci-1",
		TargetRevision: "rev-1", RunRef: "opaque-run-1", ClaimID: claim.ID,
		CompletedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil || first.VerificationStatus != "unverified" {
		t.Fatalf("first CI advisory = %#v, %v", first, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE automation_claims SET leased_until=now()-interval '1 second' WHERE id=$1", claim.ID); err != nil {
		t.Fatal(err)
	}
	replacement := claimStageOnly(t, ctx, q, org, project, issue, "ci", "rev-1", lease)
	if replacement.ID == claim.ID {
		t.Fatal("expired claim was not replaced")
	}
	second, err := q.InsertAutomationReceipt(ctx, generated.InsertAutomationReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Stage: "ci", Status: "passed", IdempotencyKey: "recover-ci-2",
		TargetRevision: "rev-1", RunRef: "opaque-run-2", ClaimID: replacement.ID,
		CompletedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil || second.VerificationStatus != "unverified" {
		t.Fatalf("retried CI advisory = %#v, %v", second, err)
	}
	stats, err := q.GetAutomationVerificationStats(ctx, generated.GetAutomationVerificationStatsParams{
		OrgID: org, ProjectID: project, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	})
	if err != nil || stats.PendingCount != 2 || stats.CiCount != 2 {
		t.Fatalf("pending verification after retry = %#v, %v", stats, err)
	}
	other, err := q.GetAutomationVerificationStats(ctx, generated.GetAutomationVerificationStatsParams{
		OrgID: otherOrg, ProjectID: otherProject, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	})
	if err != nil || other.PendingCount != 0 {
		t.Fatalf("cross-tenant verification backlog = %#v, %v", other, err)
	}
	// Only a future trusted verifier may perform this update. It proves an
	// advisory row does not block later attestation and the next stage.
	if _, err := pool.Exec(ctx, "UPDATE automation_receipts SET verification_status='verified' WHERE id=$1", second.ID); err != nil {
		t.Fatal(err)
	}
	if got := claimStageOnly(t, ctx, q, org, project, issue, "merge", "rev-1", lease); got == nil {
		t.Fatal("verified CI did not permit merge claim")
	}
}
