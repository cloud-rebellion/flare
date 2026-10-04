package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/id"
)

func envelopeBody(items ...[2]string) string {
	var b strings.Builder
	b.WriteString("{}\n")
	for _, item := range items {
		fmt.Fprintf(&b, `{"type":%q}`+"\n%s\n", item[0], item[1])
	}
	return b.String()
}

func countEnvelopeRows(t *testing.T, pool *pgxpool.Pool, table, projectID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE project_id=$1", projectID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func envelopeResult(t *testing.T, body string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode envelope response %q: %v", body, err)
	}
	return result
}

func TestEnvelopeItemFailureRollsBackWholeRequest(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules (id,org_id,project_id,name,type)
		VALUES ($1,$2,$3,'new issue','new_issue')`, id.New(), project.OrgID, project.ID); err != nil {
		t.Fatal(err)
	}
	// The transaction and first event both write successfully. The second
	// event fails inside the same database transaction, so neither earlier
	// write nor either alert evaluation may survive the 503.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_envelope_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.message = 'force-fail' THEN RAISE EXCEPTION 'test store failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_envelope_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_envelope_event()`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
	transaction := `{"event_id":"tx-one","transaction":"GET /checkout","start_timestamp":1689000000.1,"timestamp":1689000000.6,"contexts":{"trace":{"trace_id":"trace-one","span_id":"span-one","op":"http.server","status":"ok"}}}`
	body := envelopeBody(
		[2]string{"event", `{"event_id":"event-one","message":"checkout failed"}`},
		[2]string{"transaction", transaction},
		[2]string{"event", `{"event_id":"event-two","message":"force-fail"}`},
	)
	rec := sendEnvelope(s, project.PublicKey, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want retryable 503, got %d: %s", rec.Code, rec.Body.String())
	}
	result := envelopeResult(t, rec.Body.String())
	if result["accepted"] != float64(0) || result["failed"] != float64(3) || result["retryable"] != true || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("inaccurate failed envelope result: %#v", result)
	}
	for _, table := range []string{"events", "spans", "issues", "issue_alert_evaluations"} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != 0 {
			t.Fatalf("%s has %d rolled-back rows", table, got)
		}
	}
	var received, accepted, persisted int64
	var outcome string
	var retryable bool
	if err := pool.QueryRow(ctx, `SELECT received_count,accepted_count,persisted_count,outcome,retryable
		FROM ingest_receipts WHERE org_id=$1 AND project_id=$2 ORDER BY received_at DESC LIMIT 1`, project.OrgID, project.ID).
		Scan(&received, &accepted, &persisted, &outcome, &retryable); err != nil {
		t.Fatal(err)
	}
	if received != 3 || accepted != 0 || persisted != 0 || outcome != "failed" || !retryable {
		t.Fatalf("failed receipt = %d/%d/%d %s %t", received, accepted, persisted, outcome, retryable)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_envelope_event ON events; DROP FUNCTION fail_envelope_event()`); err != nil {
		t.Fatalf("remove failure trigger: %v", err)
	}
	rec = sendEnvelope(s, project.PublicKey, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry should commit every supported item, got %d: %s", rec.Code, rec.Body.String())
	}
	for table, want := range map[string]int{"events": 2, "spans": 1, "issues": 2, "issue_alert_evaluations": 2} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != want {
			t.Fatalf("%s has %d rows after retry, want %d", table, got, want)
		}
	}
}

func TestEnvelopeSameIssueMarksOnlyFirstEventNew(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules (id,org_id,project_id,name,type)
		VALUES ($1,$2,$3,'new issue','new_issue')`, id.New(), project.OrgID, project.ID); err != nil {
		t.Fatal(err)
	}
	body := envelopeBody(
		[2]string{"event", `{"event_id":"same-1","message":"same issue"}`},
		[2]string{"event", `{"event_id":"same-2","message":"same issue"}`},
	)
	rec := sendEnvelope(s, project.PublicKey, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-issue envelope: %d %s", rec.Code, rec.Body.String())
	}
	if got := countEnvelopeRows(t, pool, "events", project.ID); got != 2 {
		t.Fatalf("stored events = %d, want 2", got)
	}
	var newJobs, count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE is_new), max(event_count)
		FROM issue_alert_evaluations WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).Scan(&newJobs, &count); err != nil {
		t.Fatal(err)
	}
	if newJobs != 1 || count != 1 {
		t.Fatalf("new-issue evaluations = %d, first snapshot = %d; want 1/1", newJobs, count)
	}
	var issueCount int
	if err := pool.QueryRow(ctx, `SELECT event_count FROM issues WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).Scan(&issueCount); err != nil || issueCount != 2 {
		t.Fatalf("issue event_count = %d: %v", issueCount, err)
	}
}

func TestEnvelopeSpanStoreFailureRollsBackEarlierEvent(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	// Authentication, receipt, issue and event writes still work, while the
	// transaction item's CopyFrom fails after the earlier event was written.
	if _, err := pool.Exec(ctx, "DROP TABLE spans CASCADE"); err != nil {
		t.Fatalf("break span store: %v", err)
	}
	transaction := `{"event_id":"tx-one","transaction":"GET /checkout","start_timestamp":1689000000.1,"timestamp":1689000000.6,"contexts":{"trace":{"trace_id":"trace-one","span_id":"span-one","op":"http.server","status":"ok"}}}`
	body := envelopeBody(
		[2]string{"event", `{"event_id":"event-one","message":"checkout failed"}`},
		[2]string{"transaction", transaction},
	)
	rec := sendEnvelope(s, project.PublicKey, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("span store failure: want 503, got %d %s", rec.Code, rec.Body.String())
	}
	result := envelopeResult(t, rec.Body.String())
	if result["accepted"] != float64(0) || result["failed"] != float64(2) {
		t.Fatalf("span failure result = %#v", result)
	}
	for _, table := range []string{"events", "issues", "issue_alert_evaluations"} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != 0 {
			t.Fatalf("%s has %d rolled-back rows", table, got)
		}
	}
}

func TestEnvelopePartialInvalidIsExplicitAndReceipted(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := envelopeBody(
		[2]string{"event", `{"event_id":"valid-one","message":"works"}`},
		[2]string{"event", `not-json`},
	)
	rec := sendEnvelope(s, project.PublicKey, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial envelope: %d %s", rec.Code, rec.Body.String())
	}
	result := envelopeResult(t, rec.Body.String())
	if result["received"] != float64(2) || result["accepted"] != float64(1) || result["invalid"] != float64(1) || result["partial"] != true || result["retryable"] != false {
		t.Fatalf("partial body = %#v", result)
	}
	if got := countEnvelopeRows(t, pool, "events", project.ID); got != 1 {
		t.Fatalf("stored events = %d, want 1", got)
	}
	var accepted, persisted, rejected int64
	var outcome string
	if err := pool.QueryRow(context.Background(), `SELECT accepted_count,persisted_count,rejected_count,outcome
		FROM ingest_receipts WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).
		Scan(&accepted, &persisted, &rejected, &outcome); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || persisted != 1 || rejected != 1 || outcome != "partial" {
		t.Fatalf("partial receipt = %d/%d/%d %s", accepted, persisted, rejected, outcome)
	}
}
