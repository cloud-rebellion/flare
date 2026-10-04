package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/bright-interaction/flare/internal/id"
)

// These tests run against a fresh PostgreSQL 16 database when
// FLARE_TEST_DATABASE_URL is set. They exercise the public Store handler and
// its receipt, item claim, issue, event, and alert-evaluation transaction.
func TestStoreReplayIdentityAndProjectScope(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	first := seedProjectForIngest(t, pool)
	second := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules (id,org_id,project_id,name,type)
		VALUES ($1,$2,$3,'new issue','new_issue')`, id.New(), first.OrgID, first.ID); err != nil {
		t.Fatal(err)
	}
	body := `{"event_id":"store-replay","message":"checkout failed"}`
	for i := 0; i < 2; i++ {
		rec := storeEvent(s, first.PublicKey, body)
		if rec.Code != http.StatusOK || rec.Body.String() != "{\"id\":\"store-replay\"}\n" {
			t.Fatalf("Store attempt %d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	s.bgWG.Wait()
	for table, want := range map[string]int{
		"events": 1, "issues": 1, "issue_alert_evaluations": 1, "sentry_item_acceptances": 1,
	} {
		if got := countEnvelopeRows(t, pool, table, first.ID); got != want {
			t.Fatalf("first project %s rows = %d, want %d", table, got, want)
		}
	}
	var issueCount int
	if err := pool.QueryRow(ctx, `SELECT event_count FROM issues WHERE project_id=$1`, first.ID).Scan(&issueCount); err != nil || issueCount != 1 {
		t.Fatalf("issue count after retry = %d: %v", issueCount, err)
	}
	var accepted, persisted int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE persisted_count=1)
		FROM ingest_receipts WHERE project_id=$1 AND pillar='event' AND protocol='sentry'
		AND outcome='accepted' AND accepted_count=1`, first.ID).Scan(&accepted, &persisted); err != nil || accepted != 2 || persisted != 1 {
		t.Fatalf("Store acceptance receipts = %d accepted, %d persisted: %v", accepted, persisted, err)
	}
	changed := storeEvent(s, first.PublicKey, `{"event_id":"store-replay","message":"different issue"}`)
	if changed.Code != http.StatusConflict {
		t.Fatalf("reused ID with different payload = %d %s", changed.Code, changed.Body.String())
	}
	if got := countEnvelopeRows(t, pool, "events", first.ID); got != 1 {
		t.Fatalf("conflict persisted %d events", got)
	}
	// A second tenant can use the same ID for its own distinct event.
	other := storeEvent(s, second.PublicKey, `{"event_id":"store-replay","message":"other tenant"}`)
	if other.Code != http.StatusOK || countEnvelopeRows(t, pool, "events", second.ID) != 1 {
		t.Fatalf("second project event = %d %s", other.Code, other.Body.String())
	}
	// A missing ID is accepted with a fresh generated ID on each submission.
	keyless := `{"message":"independent keyless event"}`
	var ids [2]string
	for i := range ids {
		rec := storeEvent(s, first.PublicKey, keyless)
		if rec.Code != http.StatusOK {
			t.Fatalf("keyless attempt %d = %d %s", i, rec.Code, rec.Body.String())
		}
		var response struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.ID == "" {
			t.Fatalf("keyless response %d = %s: %v", i, rec.Body.String(), err)
		}
		ids[i] = response.ID
	}
	if ids[0] == ids[1] || countEnvelopeRows(t, pool, "events", first.ID) != 3 || countEnvelopeRows(t, pool, "sentry_item_acceptances", first.ID) != 1 {
		t.Fatalf("keyless semantics changed: ids=%v", ids)
	}
}

func TestStoreConcurrentExactRetryCommitsOnce(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	const attempts = 6
	statuses := make(chan int, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- storeEvent(s, project.PublicKey, `{"event_id":"concurrent-store","message":"one failure"}`).Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent Store status = %d", status)
		}
	}
	s.bgWG.Wait()
	for table, want := range map[string]int{"events": 1, "issues": 1, "sentry_item_acceptances": 1} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != want {
			t.Fatalf("concurrent %s rows = %d, want %d", table, got, want)
		}
	}
	var accepted, persisted int
	if err := pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE persisted_count=1)
		FROM ingest_receipts WHERE project_id=$1 AND pillar='event' AND protocol='sentry'
		AND outcome='accepted' AND accepted_count=1`, project.ID).Scan(&accepted, &persisted); err != nil || accepted != attempts || persisted != 1 {
		t.Fatalf("concurrent receipt counts = %d/%d: %v", accepted, persisted, err)
	}
}

func TestStoreAndEnvelopeShareEventReplayClaim(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := `{"event_id":"cross-protocol","message":"same event"}`
	if rec := sendEnvelope(s, project.PublicKey, envelopeBody([2]string{"event", body})); rec.Code != http.StatusOK {
		t.Fatalf("envelope first = %d %s", rec.Code, rec.Body.String())
	}
	if rec := storeEvent(s, project.PublicKey, body); rec.Code != http.StatusOK {
		t.Fatalf("Store replay = %d %s", rec.Code, rec.Body.String())
	}
	s.bgWG.Wait()
	if countEnvelopeRows(t, pool, "events", project.ID) != 1 || countEnvelopeRows(t, pool, "sentry_item_acceptances", project.ID) != 1 {
		t.Fatal("cross-protocol replay persisted a second event or identity")
	}
	var deduplicated int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM ingest_receipts
		WHERE project_id=$1 AND pillar='event' AND protocol='sentry'
		AND outcome='accepted' AND accepted_count=1 AND persisted_count=0`, project.ID).Scan(&deduplicated); err != nil || deduplicated != 1 {
		t.Fatalf("cross-protocol Store replay receipt count = %d: %v", deduplicated, err)
	}
}

func TestStoreReplayReceiptFailureDoesNotAcceptClaim(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := `{"event_id":"receipt-retry","message":"atomic failure"}`
	installFailure := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_store_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.protocol='sentry' THEN RAISE EXCEPTION 'forced terminal receipt failure'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER fail_store_receipt BEFORE UPDATE ON ingest_receipts
			FOR EACH ROW EXECUTE FUNCTION fail_store_receipt()`); err != nil {
			t.Fatal(err)
		}
	}
	removeFailure := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `DROP TRIGGER fail_store_receipt ON ingest_receipts;
			DROP FUNCTION fail_store_receipt()`); err != nil {
			t.Fatal(err)
		}
	}
	installFailure()
	failed := storeEvent(s, project.PublicKey, body)
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("Retry-After") != "1" {
		t.Fatalf("initial receipt failure = %d %s", failed.Code, failed.Body.String())
	}
	for _, table := range []string{"events", "issues", "sentry_item_acceptances"} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != 0 {
			t.Fatalf("%s survived failed receipt: %d", table, got)
		}
	}
	removeFailure()
	if rec := storeEvent(s, project.PublicKey, body); rec.Code != http.StatusOK {
		t.Fatalf("retry after rollback = %d %s", rec.Code, rec.Body.String())
	}
	installFailure()
	failed = storeEvent(s, project.PublicKey, body)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("replay receipt failure = %d %s", failed.Code, failed.Body.String())
	}
	for _, table := range []string{"events", "issues", "sentry_item_acceptances"} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != 1 {
			t.Fatalf("%s changed after failed replay: %d", table, got)
		}
	}
	removeFailure()
	s.bgWG.Wait()
}
