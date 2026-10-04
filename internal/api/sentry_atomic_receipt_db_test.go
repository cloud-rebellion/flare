package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func sentryReceiptCount(t *testing.T, pool *pgxpool.Pool, project *generated.Project, pillar, outcome string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM ingest_receipts
		WHERE org_id=$1 AND project_id=$2 AND protocol='sentry' AND pillar=$3 AND outcome=$4`,
		project.OrgID, project.ID, pillar, outcome).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// A Sentry 200 must include its terminal receipt. A failed receipt update
// after the payload and replay claims were written must roll the whole request
// back, return a retryable 503, and leave only the started audit receipt.
func TestSentryPayloadAndTerminalReceiptCommitTogether(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules (id,org_id,project_id,name,type)
		VALUES ($1,$2,$3,'new issue','new_issue')`, id.New(), project.OrgID, project.ID); err != nil {
		t.Fatal(err)
	}
	installReceiptFailure := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_sentry_terminal_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.protocol='sentry' THEN RAISE EXCEPTION 'forced receipt failure'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER fail_sentry_terminal_receipt BEFORE UPDATE ON ingest_receipts
			FOR EACH ROW EXECUTE FUNCTION fail_sentry_terminal_receipt()`); err != nil {
			t.Fatal(err)
		}
	}
	removeReceiptFailure := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `DROP TRIGGER fail_sentry_terminal_receipt ON ingest_receipts;
			DROP FUNCTION fail_sentry_terminal_receipt()`); err != nil {
			t.Fatal(err)
		}
	}
	storeBody := `{"event_id":"store-atomic","message":"store atomic"}`
	transaction := `{"event_id":"tx-atomic","transaction":"GET /checkout","start_timestamp":1689000000.1,"timestamp":1689000000.6,"contexts":{"trace":{"trace_id":"trace-atomic","span_id":"span-atomic","op":"http.server","status":"ok"}}}`
	envelope := envelopeBody(
		[2]string{"event", `{"event_id":"envelope-atomic","message":"envelope atomic"}`},
		[2]string{"transaction", transaction},
	)
	installReceiptFailure()
	store := storeEvent(s, project.PublicKey, storeBody)
	if store.Code != http.StatusServiceUnavailable || store.Header().Get("Retry-After") != "1" {
		t.Fatalf("store receipt failure = %d %s", store.Code, store.Body.String())
	}
	env := sendEnvelope(s, project.PublicKey, envelope)
	if env.Code != http.StatusServiceUnavailable || env.Header().Get("Retry-After") != "1" {
		t.Fatalf("envelope receipt failure = %d %s", env.Code, env.Body.String())
	}
	result := envelopeResult(t, env.Body.String())
	if result["accepted"] != float64(0) || result["failed"] != float64(2) || result["retryable"] != true {
		t.Fatalf("failed envelope response = %#v", result)
	}
	for _, table := range []string{"events", "issues", "spans", "issue_alert_evaluations", "sentry_item_acceptances"} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != 0 {
			t.Fatalf("%s retained %d rows after receipt failure", table, got)
		}
	}
	for _, pillar := range []string{"event", "envelope"} {
		if got := sentryReceiptCount(t, pool, project, pillar, "in_progress"); got != 1 {
			t.Fatalf("%s pending receipts = %d, want 1", pillar, got)
		}
	}
	// Even a valid empty envelope cannot receive a 200 without a terminal row.
	empty := sendEnvelope(s, project.PublicKey, "{}\n")
	if empty.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty envelope receipt failure = %d %s", empty.Code, empty.Body.String())
	}
	removeReceiptFailure()

	store = storeEvent(s, project.PublicKey, storeBody)
	env = sendEnvelope(s, project.PublicKey, envelope)
	empty = sendEnvelope(s, project.PublicKey, "{}\n")
	if store.Code != http.StatusOK || env.Code != http.StatusOK || empty.Code != http.StatusOK {
		t.Fatalf("retry statuses store/envelope/empty = %d/%d/%d", store.Code, env.Code, empty.Code)
	}
	s.bgWG.Wait()
	// The supplied Store event ID now claims the same replay ledger as the
	// envelope event and transaction IDs, yielding three accepted items.
	for table, want := range map[string]int{
		"events": 2, "issues": 2, "spans": 1, "issue_alert_evaluations": 2, "sentry_item_acceptances": 3,
	} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != want {
			t.Fatalf("%s rows after retry = %d, want %d", table, got, want)
		}
	}
	for _, tc := range []struct {
		pillar              string
		accepted, persisted int64
	}{
		{"event", 1, 1}, {"envelope", 2, 2},
	} {
		var accepted, persisted int64
		var status int16
		var outcome string
		var completed, retryable bool
		if err := pool.QueryRow(ctx, `SELECT accepted_count,persisted_count,http_status,outcome,
			completed_at IS NOT NULL,retryable FROM ingest_receipts
			WHERE org_id=$1 AND project_id=$2 AND protocol='sentry' AND pillar=$3 AND outcome='accepted'
			ORDER BY received_at DESC,id DESC LIMIT 1`, project.OrgID, project.ID, tc.pillar).
			Scan(&accepted, &persisted, &status, &outcome, &completed, &retryable); err != nil {
			t.Fatal(err)
		}
		if accepted != tc.accepted || persisted != tc.persisted || status != 200 || outcome != "accepted" || !completed || retryable {
			t.Fatalf("%s terminal receipt = %d/%d %d %s completed=%t retryable=%t",
				tc.pillar, accepted, persisted, status, outcome, completed, retryable)
		}
	}
	if got := sentryReceiptCount(t, pool, project, "envelope", "empty"); got != 1 {
		t.Fatalf("empty terminal receipt count = %d, want 1", got)
	}
	// The replay path still needs its own terminal receipt. Failure must return
	// 503 while leaving the original event, spans, and item ledger unchanged.
	installReceiptFailure()
	replay := sendEnvelope(s, project.PublicKey, envelope)
	if replay.Code != http.StatusServiceUnavailable {
		t.Fatalf("replay receipt failure = %d %s", replay.Code, replay.Body.String())
	}
	for table, want := range map[string]int{"events": 2, "spans": 1, "sentry_item_acceptances": 3} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != want {
			t.Fatalf("replay changed %s rows to %d, want %d", table, got, want)
		}
	}
	if got := sentryReceiptCount(t, pool, project, "envelope", "in_progress"); got != 3 {
		t.Fatalf("envelope pending receipts = %d, want 3 (first failure, empty failure, replay failure)", got)
	}
}
