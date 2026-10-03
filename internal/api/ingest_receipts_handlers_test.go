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

func TestIngestReceiptLimitIsBounded(t *testing.T) {
	tests := []struct {
		query string
		want  int32
	}{
		{"", defaultIngestReceiptLimit},
		{"?limit=25", 25},
		{"?limit=5000", maxIngestReceiptLimit},
		{"?limit=0", defaultIngestReceiptLimit},
		{"?limit=bad", defaultIngestReceiptLimit},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/projects/p/ingest-receipts"+tc.query, nil)
			if got := ingestReceiptLimit(r); got != tc.want {
				t.Fatalf("ingestReceiptLimit(%q) = %d, want %d", tc.query, got, tc.want)
			}
		})
	}
}

func TestIngestReceiptResponseIsPayloadFree(t *testing.T) {
	received := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	completed := received.Add(time.Second)
	row := &generated.IngestReceipt{
		ID:             "receipt-1",
		RequestID:      pgtype.Text{String: "req-1", Valid: true},
		OrgID:          "org-secret",
		ProjectID:      "project-secret",
		Pillar:         "logs",
		Protocol:       "otlp",
		ReceivedCount:  3,
		AcceptedCount:  2,
		RejectedCount:  1,
		DroppedCount:   0,
		PersistedCount: 2,
		Outcome:        "partial",
		HttpStatus:     http.StatusOK,
		Retryable:      false,
		ReceivedAt:     pgtype.Timestamptz{Time: received, Valid: true},
		CompletedAt:    pgtype.Timestamptz{Time: completed, Valid: true},
		CreatedAt:      pgtype.Timestamptz{Time: received, Valid: true},
	}
	out := toIngestReceiptResponse(row)
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal receipt response: %v", err)
	}
	text := string(body)
	for _, forbidden := range []string{"org-secret", "project-secret", "payload", "body", "credential"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("receipt response leaked %q: %s", forbidden, text)
		}
	}
	for _, want := range []string{`"id":"receipt-1"`, `"request_id":"req-1"`, `"accepted_count":2`, `"outcome":"partial"`, `"http_status":200`} {
		if !strings.Contains(text, want) {
			t.Fatalf("receipt response missing %q: %s", want, text)
		}
	}
	if out.ReceivedAt == nil || !out.ReceivedAt.Equal(received) || out.CompletedAt == nil || !out.CompletedAt.Equal(completed) {
		t.Fatalf("receipt timestamps = %#v, want received/completed times", out)
	}
}

func TestIngestReceiptRouteIsAuthenticatedViewerRead(t *testing.T) {
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
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/ingest-receipts", s.handleListIngestReceipts)`) {
		t.Fatal("ingest receipt route is not in the authenticated viewer read group")
	}
}
