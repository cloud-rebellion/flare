package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func sendOTLPJSON(s *Server, p *generated.Project, path, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flare-Key", p.PublicKey)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func otlpRowCount(t *testing.T, pool *pgxpool.Pool, table string, p *generated.Project) int {
	t.Helper()
	var n int
	// Table is a constant from this test's case list, never user input.
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A 200 must represent one committed payload plus its completed receipt. If
// the receipt update fails after CopyFrom, the payload must roll back and the
// handler must return a retryable 503. The previously started receipt remains
// in_progress for the acceptance watchdog; it never falsely says accepted.
func TestOTLPPayloadAndTerminalReceiptCommitTogether(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	traceID := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 16)) }
	spanID := base64.StdEncoding.EncodeToString([]byte{1, 1, 1, 1, 1, 1, 1, 1})
	traceBody := func(id string) string {
		return fmt.Sprintf(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":%q,"spanId":%q,"name":"GET /checkout"}]}]}]}`, id, spanID)
	}
	cases := []struct {
		pillar, table, path, successBody, failureBody string
		handler                                       func(http.ResponseWriter, *http.Request)
	}{
		{"logs", "logs", "/otlp/v1/logs",
			`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"ok"},"severityText":"INFO"}]}]}]}`,
			`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"later"},"severityText":"WARN"}]}]}]}`,
			s.handleOTLPLogs},
		{"spans", "spans", "/otlp/v1/traces", traceBody(traceID(1)), traceBody(traceID(2)), s.handleOTLPTraces},
		{"metrics", "metrics", "/otlp/v1/metrics",
			`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"requests","gauge":{"dataPoints":[{"asDouble":1}]}}]}]}]}`,
			`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"requests_later","gauge":{"dataPoints":[{"asDouble":2}]}}]}]}]}`,
			s.handleOTLPMetrics},
	}
	for _, tc := range cases {
		t.Run(tc.pillar+" accepted", func(t *testing.T) {
			rec := sendOTLPJSON(s, p, tc.path, tc.successBody, tc.handler)
			if rec.Code != http.StatusOK {
				t.Fatalf("OTLP response = %d %s", rec.Code, rec.Body.String())
			}
			if got := otlpRowCount(t, pool, tc.table, p); got != 1 {
				t.Fatalf("stored %s = %d, want 1", tc.table, got)
			}
			var received, accepted, persisted, rejected, dropped int64
			var status int16
			var outcome string
			var completed, retryable bool
			err := pool.QueryRow(context.Background(), `SELECT received_count,accepted_count,persisted_count,rejected_count,dropped_count,
				http_status,outcome,completed_at IS NOT NULL,retryable FROM ingest_receipts
				WHERE org_id=$1 AND project_id=$2 AND pillar=$3 AND protocol='otlp'
				ORDER BY received_at DESC,id DESC LIMIT 1`, p.OrgID, p.ID, tc.pillar).
				Scan(&received, &accepted, &persisted, &rejected, &dropped, &status, &outcome, &completed, &retryable)
			if err != nil {
				t.Fatal(err)
			}
			if received != 1 || accepted != 1 || persisted != 1 || rejected != 0 || dropped != 0 ||
				status != 200 || outcome != "accepted" || !completed || retryable {
				t.Fatalf("terminal OTLP receipt = %d/%d/%d/%d/%d status=%d %s completed=%t retryable=%t",
					received, accepted, persisted, rejected, dropped, status, outcome, completed, retryable)
			}
			bad := sendOTLPJSON(s, p, tc.path, "not-json", tc.handler)
			if bad.Code != http.StatusBadRequest {
				t.Fatalf("malformed OTLP response = %d %s", bad.Code, bad.Body.String())
			}
			var rejectedReceipts int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM ingest_receipts
				WHERE org_id=$1 AND project_id=$2 AND pillar=$3 AND protocol='otlp'
				  AND outcome='rejected' AND http_status=400 AND completed_at IS NOT NULL`,
				p.OrgID, p.ID, tc.pillar).Scan(&rejectedReceipts); err != nil {
				t.Fatal(err)
			}
			if rejectedReceipts != 1 {
				t.Fatalf("malformed OTLP rejection receipts = %d, want 1", rejectedReceipts)
			}
		})
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_otlp_terminal_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.protocol='otlp' THEN RAISE EXCEPTION 'forced receipt failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_otlp_terminal_receipt BEFORE UPDATE ON ingest_receipts
		FOR EACH ROW EXECUTE FUNCTION fail_otlp_terminal_receipt()`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.pillar+" rollback", func(t *testing.T) {
			rec := sendOTLPJSON(s, p, tc.path, tc.failureBody, tc.handler)
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("failed OTLP response = %d %s", rec.Code, rec.Body.String())
			}
			if got := otlpRowCount(t, pool, tc.table, p); got != 1 {
				t.Fatalf("receipt failure left %d stored %s rows, want original 1", got, tc.table)
			}
			var pending int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM ingest_receipts
				WHERE org_id=$1 AND project_id=$2 AND pillar=$3 AND protocol='otlp'
				  AND outcome='in_progress' AND completed_at IS NULL`, p.OrgID, p.ID, tc.pillar).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != 1 {
				t.Fatalf("pending receipt count %d, want 1", pending)
			}
		})
	}
}
