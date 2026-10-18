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

func TestAIActionReceiptLimitIsBounded(t *testing.T) {
	tests := []struct {
		query string
		want  int32
	}{
		{"", defaultAIActionReceiptLimit},
		{"?limit=25", 25},
		{"?limit=5000", maxAIActionReceiptLimit},
		{"?limit=0", defaultAIActionReceiptLimit},
		{"?limit=bad", defaultAIActionReceiptLimit},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/projects/p/ai-receipts"+tc.query, nil)
			if got := aiActionReceiptLimit(r); got != tc.want {
				t.Fatalf("aiActionReceiptLimit(%q) = %d, want %d", tc.query, got, tc.want)
			}
		})
	}
}

func TestAIActionReceiptResponseIsPayloadFree(t *testing.T) {
	started := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	completed := started.Add(time.Second)
	row := &generated.AiActionReceipt{
		ID:             "receipt-1",
		OrgID:          "org-secret",
		ProjectID:      "project-secret",
		IssueID:        "issue-1",
		Operation:      "triage",
		Status:         "failed",
		Provider:       "openai",
		StartedAt:      pgtype.Timestamptz{Time: started, Valid: true},
		CompletedAt:    pgtype.Timestamptz{Time: completed, Valid: true},
		Retryable:      true,
		ErrorCode:      pgtype.Text{String: "provider_error", Valid: true},
		EvidenceDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CreatedAt:      pgtype.Timestamptz{Time: started, Valid: true},
	}
	out := toAIActionReceiptResponse(row)
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal receipt response: %v", err)
	}
	text := string(body)
	for _, forbidden := range []string{"org-secret", "project-secret", "prompt", "completion", "credential", "provider detail"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("receipt response leaked %q: %s", forbidden, text)
		}
	}
	for _, want := range []string{`"id":"receipt-1"`, `"issue_id":"issue-1"`, `"operation":"triage"`, `"status":"failed"`, `"provider":"openai"`, `"evidence_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`, `"error_code":"provider_error"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("receipt response missing %q: %s", want, text)
		}
	}
	if out.StartedAt == nil || !out.StartedAt.Equal(started) || out.CompletedAt == nil || !out.CompletedAt.Equal(completed) {
		t.Fatalf("receipt timestamps = %#v, want started/completed times", out)
	}
}

func TestAIActionReceiptRouteIsAuthenticatedViewerRead(t *testing.T) {
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
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/ai-receipts", s.handleListAIActionReceipts)`) {
		t.Fatal("AI action receipt route is not in the authenticated viewer read group")
	}
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/ai-receipts/{receiptID}", s.handleGetAIActionReceipt)`) {
		t.Fatal("exact AI action receipt route is not in the authenticated viewer read group")
	}
}
