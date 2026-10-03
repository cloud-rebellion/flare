package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

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
				{ID: "synthetic-1", IssueID: "issue-1", Stage: "synthetic", Status: "passed", TargetRevision: "rev-1", RunRef: "probe-1"},
			}, state: "blocked", stage: "synthetic", prereqID: "deploy-1",
		},
		{
			name: "wrong revision cannot satisfy", rows: []*generated.AutomationReceipt{
				automationNextRow("patch-1", "patch", "passed", "rev-1"),
				automationNextRow("review-2", "review", "approved", "rev-2"),
			}, state: "not_started",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := automationNextAction("issue-1", tc.rows)
			if got.Available != tc.available || got.State != tc.state || got.Stage != tc.stage || got.PrerequisiteReceiptID != tc.prereqID {
				t.Fatalf("next action = %#v, want available=%v state=%q stage=%q prerequisite=%q", got, tc.available, tc.state, tc.stage, tc.prereqID)
			}
			if got.IssueID != "issue-1" || (got.TargetRevision != "" && got.TargetRevision != "rev-1" && tc.name != "wrong revision cannot satisfy") {
				t.Fatalf("next action provenance = %#v", got)
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
