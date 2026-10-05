package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func TestAIActionReceiptScopeAndLifecycle(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	otherOrg, _ := seedProject(t, ctx, q)
	issue := seedIssue(t, ctx, q, org, project, "triage me", "error")
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	const failedEvidenceDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	receipt, err := q.InsertAIActionReceipt(ctx, generated.InsertAIActionReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Operation: "triage", Status: "started", Provider: "unknown",
		StartedAt: now, Retryable: true,
		EvidenceDigest: failedEvidenceDigest,
	})
	if err != nil {
		t.Fatalf("insert AI action receipt: %v", err)
	}
	if receipt.Status != "started" || receipt.IssueID != issue || receipt.EvidenceDigest == "" {
		t.Fatalf("inserted receipt = %#v", receipt)
	}

	completed, err := q.CompleteAIActionReceipt(ctx, generated.CompleteAIActionReceiptParams{
		ID: receipt.ID, OrgID: org, ProjectID: project,
		Status: "failed", Provider: "openai", CompletedAt: now,
		Retryable: true, ErrorCode: pgtype.Text{String: "provider_error", Valid: true},
		EvidenceDigest: receipt.EvidenceDigest,
	})
	if err != nil {
		t.Fatalf("complete AI action receipt: %v", err)
	}
	if completed.Status != "failed" || !completed.ErrorCode.Valid || completed.ErrorCode.String != "provider_error" {
		t.Fatalf("completed receipt = %#v", completed)
	}
	const evidenceDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	succeeded, err := q.InsertAIActionReceipt(ctx, generated.InsertAIActionReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Operation: "triage", Status: "succeeded", Provider: "openai",
		StartedAt: now, Retryable: false, EvidenceDigest: evidenceDigest,
	})
	if err != nil {
		t.Fatalf("insert successful AI action receipt: %v", err)
	}
	if _, err := q.CompleteAIActionReceipt(ctx, generated.CompleteAIActionReceiptParams{
		ID: succeeded.ID, OrgID: org, ProjectID: project,
		Status: "succeeded", Provider: "openai", CompletedAt: now,
		Retryable: false, EvidenceDigest: evidenceDigest,
	}); err != nil {
		t.Fatalf("complete successful AI action receipt: %v", err)
	}
	// A refresh can analyze unchanged evidence again. The digest identifies
	// the evidence snapshot, while the durable row ID identifies one run.
	repeated, err := q.InsertAIActionReceipt(ctx, generated.InsertAIActionReceiptParams{
		ID: id.New(), OrgID: org, ProjectID: project, IssueID: issue,
		Operation: "triage", Status: "started", Provider: "unknown",
		StartedAt: now, Retryable: true, EvidenceDigest: evidenceDigest,
	})
	if err != nil {
		t.Fatalf("insert repeated AI action receipt: %v", err)
	}
	if repeated.ID == succeeded.ID || repeated.EvidenceDigest != succeeded.EvidenceDigest {
		t.Fatalf("repeated run lost distinct ID or shared digest: first=%#v repeated=%#v", succeeded, repeated)
	}
	evidence, err := q.HasSuccessfulAIActionEvidence(ctx, generated.HasSuccessfulAIActionEvidenceParams{
		OrgID: org, ProjectID: project, IssueID: issue, EvidenceDigest: evidenceDigest,
	})
	if err != nil || !evidence {
		t.Fatalf("successful AI evidence = %v, %v; want true", evidence, err)
	}
	failedEvidence, err := q.HasSuccessfulAIActionEvidence(ctx, generated.HasSuccessfulAIActionEvidenceParams{
		OrgID: org, ProjectID: project, IssueID: issue, EvidenceDigest: failedEvidenceDigest,
	})
	if err != nil {
		t.Fatalf("failed-receipt evidence query: %v", err)
	}
	if failedEvidence {
		t.Fatal("failed AI receipt was accepted as patch evidence")
	}
	wrongDigest, err := q.HasSuccessfulAIActionEvidence(ctx, generated.HasSuccessfulAIActionEvidenceParams{
		OrgID: org, ProjectID: project, IssueID: issue, EvidenceDigest: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd",
	})
	if err != nil {
		t.Fatalf("wrong-digest AI evidence query: %v", err)
	}
	if wrongDigest {
		t.Fatal("wrong AI evidence digest was accepted")
	}
	crossTenantEvidence, err := q.HasSuccessfulAIActionEvidence(ctx, generated.HasSuccessfulAIActionEvidenceParams{
		OrgID: otherOrg, ProjectID: project, IssueID: issue, EvidenceDigest: evidenceDigest,
	})
	if err != nil {
		t.Fatalf("cross-tenant AI evidence query: %v", err)
	}
	if crossTenantEvidence {
		t.Fatal("cross-tenant AI evidence was visible")
	}

	rows, err := q.ListAIActionReceiptsByProject(ctx, generated.ListAIActionReceiptsByProjectParams{
		OrgID: org, ProjectID: project, Limit: 10,
	})
	if err != nil || len(rows) != 3 {
		t.Fatalf("scoped list = %v (%d rows), want three receipts", err, len(rows))
	}
	foundFirst, foundRepeated := false, false
	for _, row := range rows {
		foundFirst = foundFirst || row.ID == succeeded.ID
		foundRepeated = foundRepeated || row.ID == repeated.ID
	}
	if !foundFirst || !foundRepeated {
		t.Fatalf("scoped list lost exact run IDs: first=%v repeated=%v", foundFirst, foundRepeated)
	}
	crossTenant, err := q.ListAIActionReceiptsByProject(ctx, generated.ListAIActionReceiptsByProjectParams{
		OrgID: otherOrg, ProjectID: project, Limit: 10,
	})
	if err != nil {
		t.Fatalf("cross-tenant list: %v", err)
	}
	if len(crossTenant) != 0 {
		t.Fatalf("cross-tenant list returned %d rows", len(crossTenant))
	}
	_ = pool
}
