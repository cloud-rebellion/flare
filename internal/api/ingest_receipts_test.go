package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNewIngestReceiptIsTenantScopedAndCanonical(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/ingest", nil)
	w := httptest.NewRecorder()
	r.Header.Set("X-Request-ID", "req-123")
	project := &generated.Project{ID: "project-7", OrgID: "org-9"}

	receipt := newIngestReceipt(w, r, project, "untrusted-pillar", "untrusted-protocol")
	if receipt == nil {
		t.Fatal("newIngestReceipt returned nil")
	}
	if receipt.orgID != project.OrgID || receipt.projectID != project.ID {
		t.Fatalf("receipt tenant = %q/%q, want %q/%q", receipt.orgID, receipt.projectID, project.OrgID, project.ID)
	}
	if receipt.pillar != "other" || receipt.protocol != "other" {
		t.Fatalf("receipt labels = %q/%q, want canonical other/other", receipt.pillar, receipt.protocol)
	}
	if receipt.requestID.String != "req-123" || !receipt.requestID.Valid {
		t.Fatalf("request id = %#v, want valid req-123", receipt.requestID)
	}
	if newIngestReceipt(w, r, nil, "event", "sentry") != nil {
		t.Fatal("nil project must not produce a receipt")
	}
}

func TestSanitizeReceiptRequestIDBoundsAndDropsControls(t *testing.T) {
	if got := sanitizeReceiptRequestID(" req/one?\n\t"); got != "reqone" {
		t.Fatalf("sanitized request id = %q, want reqone", got)
	}
	if got := sanitizeReceiptRequestID(strings.Repeat("é", maxReceiptRequestID)); len(got) > maxReceiptRequestID {
		t.Fatalf("sanitized request id is %d bytes, max %d", len(got), maxReceiptRequestID)
	}
	if got := sanitizeReceiptRequestID("\x00\x01"); got != "" {
		t.Fatalf("control-only request id = %q, want empty", got)
	}
}

func TestReceiptRequestIDGeneratesWhenMissing(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/ingest", nil)
	id := receiptRequestID(nil, r)
	if !id.Valid || id.String == "" {
		t.Fatalf("missing request id = %#v, want generated valid id", id)
	}
}

func TestRequireIngestReceiptFailsClosedWhenStoreUnavailable(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/ingest", nil)
	w := httptest.NewRecorder()
	project := &generated.Project{ID: "project-7", OrgID: "org-9"}
	var outcome ingestOutcome
	requestOutcome := "rejected"
	receiptStatus := http.StatusOK

	receipt := (&Server{}).requireIngestReceipt(r.Context(), w, r, project, "event", "sentry", &outcome, &requestOutcome, &receiptStatus)
	if receipt != nil {
		t.Fatal("receipt should be nil when the receipt store is unavailable")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if outcome.failed != 1 || requestOutcome != "failed" || receiptStatus != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed accounting = outcome=%#v request=%q status=%d", outcome, requestOutcome, receiptStatus)
	}
}

func TestIngestReceiptCountsAndOutcomeCanonicalization(t *testing.T) {
	receipt := &ingestReceipt{}
	receipt.setReceived(12)
	if receipt.received != 12 {
		t.Fatalf("received = %d, want 12", receipt.received)
	}
	for _, tc := range []struct {
		outcome string
		want    string
	}{
		{"in_progress", "in_progress"},
		{"accepted", "accepted"},
		{"partial", "partial"},
		{"rejected", "rejected"},
		{"failed", "failed"},
		{"empty", "empty"},
		{"garbage", "failed"},
	} {
		if got := canonicalReceiptOutcome(tc.outcome); got != tc.want {
			t.Errorf("canonicalReceiptOutcome(%q) = %q, want %q", tc.outcome, got, tc.want)
		}
	}
}
