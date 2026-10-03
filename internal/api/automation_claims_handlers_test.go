package api

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeAutomationClaimRequestBoundsLeaseAndEvidence(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := normalizeAutomationClaimRequest(createAutomationClaimRequest{
		IssueID: "issue-1", Stage: "patch", TargetRevision: "rev-1",
		WorkerRef: "worker-a", EvidenceRef: digest,
	})
	if err != nil {
		t.Fatalf("valid patch claim rejected: %v", err)
	}
	if got.LeaseSeconds != defaultAutomationClaimLeaseSeconds || !got.LeasedUntil.After(time.Now().UTC()) {
		t.Fatalf("default lease = %#v", got)
	}
	for name, req := range map[string]createAutomationClaimRequest{
		"unknown stage":            {IssueID: "i", Stage: "review", TargetRevision: "r", WorkerRef: "w"},
		"missing worker":           {IssueID: "i", Stage: "ci", TargetRevision: "r"},
		"missing revision":         {IssueID: "i", Stage: "ci", WorkerRef: "w"},
		"patch missing evidence":   {IssueID: "i", Stage: "patch", TargetRevision: "r", WorkerRef: "w"},
		"patch malformed evidence": {IssueID: "i", Stage: "patch", TargetRevision: "r", WorkerRef: "w", EvidenceRef: "not-digest"},
		"non patch evidence":       {IssueID: "i", Stage: "ci", TargetRevision: "r", WorkerRef: "w", EvidenceRef: "unexpected"},
		"negative lease":           {IssueID: "i", Stage: "ci", TargetRevision: "r", WorkerRef: "w", LeaseSeconds: -1},
		"long worker":              {IssueID: "i", Stage: "ci", TargetRevision: "r", WorkerRef: strings.Repeat("w", maxAutomationClaimWorkerRef+1)},
		"control worker":           {IssueID: "i", Stage: "ci", TargetRevision: "r", WorkerRef: "worker\nunsafe"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeAutomationClaimRequest(req); err == nil {
				t.Fatal("claim unexpectedly accepted")
			}
		})
	}
}

func TestNormalizeAutomationReceiptRequiresClaimOnlyForSuccessfulExecution(t *testing.T) {
	base := createAutomationReceiptRequest{
		IssueID: "issue-1", Stage: "ci", IdempotencyKey: "run-1", TargetRevision: "rev-1", RunRef: "ci-1",
	}
	if _, err := normalizeAutomationReceiptRequest(createAutomationReceiptRequest{
		IssueID: base.IssueID, Stage: base.Stage, Status: "failed", IdempotencyKey: base.IdempotencyKey,
		TargetRevision: base.TargetRevision, RunRef: base.RunRef, ErrorCode: "provider_error",
	}); err != nil {
		t.Fatalf("unclaimed failure should remain recordable: %v", err)
	}
	if _, err := normalizeAutomationReceiptRequest(createAutomationReceiptRequest{
		IssueID: base.IssueID, Stage: base.Stage, Status: "passed", IdempotencyKey: base.IdempotencyKey,
		TargetRevision: base.TargetRevision, RunRef: base.RunRef,
	}); err == nil {
		t.Fatal("unclaimed successful execution accepted")
	}
	if _, err := normalizeAutomationReceiptRequest(createAutomationReceiptRequest{
		IssueID: base.IssueID, Stage: base.Stage, Status: "passed", IdempotencyKey: base.IdempotencyKey,
		TargetRevision: base.TargetRevision, RunRef: base.RunRef, ClaimID: "claim-1",
	}); err != nil {
		t.Fatalf("claimed successful execution rejected: %v", err)
	}
	if _, err := normalizeAutomationReceiptRequest(createAutomationReceiptRequest{
		IssueID: base.IssueID, Stage: "review", Status: "approved", IdempotencyKey: "review-1",
		TargetRevision: base.TargetRevision, ClaimID: "claim-1",
	}); err == nil {
		t.Fatal("claim accepted for human review")
	}
}
