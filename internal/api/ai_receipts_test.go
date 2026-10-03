package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/metrics"
	"github.com/bright-interaction/flare/internal/telemetry"
)

func TestAIEvidenceDigestIsStableAndPayloadFree(t *testing.T) {
	const report = "Error: [jwt]\nStack trace: checkout at app.js:12"
	first := aiEvidenceDigest(report)
	if first != aiEvidenceDigest(report) {
		t.Fatal("same scrubbed evidence produced different digests")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first) {
		t.Fatalf("digest = %q, want lowercase SHA-256", first)
	}
	if first == aiEvidenceDigest(report+"\nchanged") {
		t.Fatal("different evidence produced the same digest")
	}
}

func TestTriageResponseExposesFreshEvidenceOnly(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fresh := triageResponse("suggested fix", false, digest)
	if got, ok := fresh["evidence_digest"].(string); !ok || got != digest {
		t.Fatalf("fresh triage response digest = %#v, want %q", fresh["evidence_digest"], digest)
	}
	cached := triageResponse("cached suggestion", true, "")
	if _, ok := cached["evidence_digest"]; ok {
		t.Fatalf("cached triage response exposed evidence digest: %#v", cached)
	}
}

func TestRequireAIActionReceiptFailsClosedWhenStoreUnavailable(t *testing.T) {
	_, err := (&Server{}).requireAIActionReceipt(context.Background(), "org-1", "project-1", "issue-1", "triage", "unknown")
	if !errors.Is(err, errAIActionReceiptUnavailable) {
		t.Fatalf("error = %v, want errAIActionReceiptUnavailable", err)
	}
}

func TestClassifyAIActionReceipt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cached    bool
		err       error
		status    string
		retryable bool
		code      string
	}{
		{name: "success", status: "succeeded"},
		{name: "cached", cached: true, status: "cached"},
		{name: "provider", err: errTriageEndpoint, status: "failed", retryable: true, code: "provider_error"},
		{name: "budget", err: errTriageBudget, status: "failed", retryable: true, code: "budget_exhausted"},
		{name: "receipt unavailable", err: errAIActionReceiptUnavailable, status: "failed", retryable: true, code: "receipt_unavailable"},
		{name: "not configured", err: errTriageNotConfigured, status: "failed", code: "not_configured"},
		{name: "not found", err: telemetry.ErrNotFound{What: "issue"}, status: "failed", code: "issue_not_found"},
		{name: "wrapped not found", err: errors.New("database unavailable"), status: "failed", retryable: true, code: "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, retryable, code := classifyAIActionReceipt(tc.cached, tc.err)
			if status != tc.status || retryable != tc.retryable || code != tc.code {
				t.Fatalf("classifyAIActionReceipt = %q/%v/%q, want %q/%v/%q", status, retryable, code, tc.status, tc.retryable, tc.code)
			}
		})
	}
}

func TestAIActionReceiptWriteMetricsClassifyFixedOutcomes(t *testing.T) {
	previous := metrics.Default
	metrics.Default = metrics.New()
	t.Cleanup(func() { metrics.Default = previous })

	recordAIActionReceiptSuccess("insert")
	recordAIActionReceiptSuccess("complete")
	recordAIActionReceiptFailure("insert", &aiActionReceipt{})
	recordAIActionReceiptFailure("complete", &aiActionReceipt{})

	out := httptest.NewRecorder()
	metrics.Default.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		`flare_ai_action_receipts_total{operation="insert",outcome="ok"} 1`,
		`flare_ai_action_receipts_total{operation="complete",outcome="ok"} 1`,
		`flare_ai_action_receipts_total{operation="insert",outcome="error"} 1`,
		`flare_ai_action_receipts_total{operation="complete",outcome="error"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestCanonicalAIActionLabels(t *testing.T) {
	for _, tc := range []struct {
		operation string
		provider  string
		wantOp    string
		wantProv  string
	}{
		{"triage", "openai", "triage", "openai"},
		{"other", "anthropic", "other", "anthropic"},
		{"triage", "cache", "triage", "cache"},
		{"triage", "local-secret-provider", "triage", "other"},
	} {
		if got := canonicalAIActionOperation(tc.operation); got != tc.wantOp {
			t.Errorf("operation %q = %q, want %q", tc.operation, got, tc.wantOp)
		}
		if got := canonicalAIActionProvider(tc.provider); got != tc.wantProv {
			t.Errorf("provider %q = %q, want %q", tc.provider, got, tc.wantProv)
		}
	}
}
