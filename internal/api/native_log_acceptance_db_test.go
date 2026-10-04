package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ingest"
)

// These tests use a migrated throwaway PostgreSQL database when
// FLARE_TEST_DATABASE_URL is set. They prove wire replay, project isolation,
// and all-or-nothing log/receipt acceptance with real CopyFrom transactions.
func sendNativeLogs(s *Server, p *generated.Project, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/"+p.ID+"/logs", strings.NewReader(body))
	req.Header.Set("X-Flare-Key", p.PublicKey)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", p.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.handleNativeLogs(rec, req)
	return rec
}

func decodeNativeLogResult(t *testing.T, rec *httptest.ResponseRecorder) nativeLogResult {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("log response = %d %s", rec.Code, rec.Body.String())
	}
	var got nativeLogResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ReceiptID == "" {
		t.Fatalf("200 without durable receipt id: %s", rec.Body.String())
	}
	return got
}

func countNativeLogRows(t *testing.T, pool *pgxpool.Pool, p *generated.Project) (logs, receipts, keys int) {
	t.Helper()
	ctx := context.Background()
	for i, stmt := range []string{
		`SELECT count(*) FROM logs WHERE org_id=$1 AND project_id=$2`,
		`SELECT count(*) FROM ingest_receipts WHERE org_id=$1 AND project_id=$2 AND pillar='logs' AND protocol='native'`,
		`SELECT count(*) FROM native_log_acceptances WHERE org_id=$1 AND project_id=$2`,
	} {
		var n int
		if err := pool.QueryRow(ctx, stmt, p.OrgID, p.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			logs = n
		case 1:
			receipts = n
		case 2:
			keys = n
		}
	}
	return
}

func TestNativeLogAtomicIdempotentAcceptance(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := `[{"severity":"error","body":"checkout failed"},{"severity":"info","body":"payment retry"}]`
	key := "native-checkout-logs-0001"
	const workers = 8
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- sendNativeLogs(s, p, key, body)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var receiptID string
	newCount := 0
	for rec := range results {
		got := decodeNativeLogResult(t, rec)
		if got.Accepted != 2 || got.Rejected != 0 || got.Dropped != 0 {
			t.Fatalf("log counts = %+v", got)
		}
		if receiptID == "" {
			receiptID = got.ReceiptID
		} else if got.ReceiptID != receiptID {
			t.Fatalf("replay receipt %q differs from %q", got.ReceiptID, receiptID)
		}
		if rec.Header().Get("Idempotency-Replayed") != "true" {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("new commits = %d, want 1", newCount)
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, p); logs != 2 || receipts != 1 || keys != 1 {
		t.Fatalf("stored logs/receipts/keys = %d/%d/%d, want 2/1/1", logs, receipts, keys)
	}
	var received, accepted, rejected, dropped, persisted int64
	var outcome string
	var completed bool
	if err := pool.QueryRow(context.Background(), `SELECT received_count,accepted_count,rejected_count,dropped_count,persisted_count,
		outcome,completed_at IS NOT NULL FROM ingest_receipts WHERE id=$1 AND org_id=$2 AND project_id=$3`,
		receiptID, p.OrgID, p.ID).Scan(&received, &accepted, &rejected, &dropped, &persisted, &outcome, &completed); err != nil {
		t.Fatal(err)
	}
	if received != 2 || accepted != 2 || rejected != 0 || dropped != 0 || persisted != 2 || outcome != "accepted" || !completed {
		t.Fatalf("terminal receipt = %d/%d/%d/%d/%d %s %t", received, accepted, rejected, dropped, persisted, outcome, completed)
	}
	changed := sendNativeLogs(s, p, key, `[{"body":"changed"}]`)
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed bytes with same key = %d %s", changed.Code, changed.Body.String())
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, p); logs != 2 || receipts != 1 || keys != 1 {
		t.Fatalf("conflict changed storage: %d/%d/%d", logs, receipts, keys)
	}
	other, err := generated.New(pool).CreateProject(context.Background(), generated.CreateProjectParams{
		ID: id.New(), OrgID: p.OrgID, Name: "same-org-other-project", Slug: "native-log-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	second := decodeNativeLogResult(t, sendNativeLogs(s, other, key, body))
	if second.ReceiptID == receiptID {
		t.Fatal("retry key crossed project boundary")
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, other); logs != 2 || receipts != 1 || keys != 1 {
		t.Fatalf("other project storage: %d/%d/%d", logs, receipts, keys)
	}
}

func TestNativeLogAtomicRollbackAndKeylessCompatibility(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_native_log() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.body = 'blocked' THEN RAISE EXCEPTION 'forced log failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_native_log BEFORE INSERT ON logs FOR EACH ROW EXECUTE FUNCTION fail_native_log()`); err != nil {
		t.Fatal(err)
	}
	body := `[{"body":"allowed"},{"body":"blocked"}]`
	key := "native-rollback-logs-0001"
	failed := sendNativeLogs(s, p, key, body)
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("Retry-After") != "1" {
		t.Fatalf("failed acceptance = %d %s", failed.Code, failed.Body.String())
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, p); logs != 0 || receipts != 0 || keys != 0 {
		t.Fatalf("failure left partial storage: %d/%d/%d", logs, receipts, keys)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_native_log ON logs; DROP FUNCTION fail_native_log()`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_native_log_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.pillar='logs' AND NEW.protocol='native' THEN RAISE EXCEPTION 'forced receipt failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_native_log_receipt BEFORE UPDATE ON ingest_receipts FOR EACH ROW EXECUTE FUNCTION fail_native_log_receipt()`); err != nil {
		t.Fatal(err)
	}
	failed = sendNativeLogs(s, p, key, body)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("receipt completion failure = %d %s", failed.Code, failed.Body.String())
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, p); logs != 0 || receipts != 0 || keys != 0 {
		t.Fatalf("receipt failure left partial storage: %d/%d/%d", logs, receipts, keys)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_native_log_receipt ON ingest_receipts; DROP FUNCTION fail_native_log_receipt()`); err != nil {
		t.Fatal(err)
	}
	accepted := decodeNativeLogResult(t, sendNativeLogs(s, p, key, body))
	if accepted.Accepted != 2 || accepted.Rejected != 0 {
		t.Fatalf("retry counts = %+v", accepted)
	}
	legacy := `[{"body":"legacy"}]`
	first := decodeNativeLogResult(t, sendNativeLogs(s, p, "", legacy))
	second := decodeNativeLogResult(t, sendNativeLogs(s, p, "", legacy))
	if first.ReceiptID == second.ReceiptID {
		t.Fatal("keyless calls unexpectedly shared a receipt")
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, p); logs != 4 || receipts != 3 || keys != 1 {
		t.Fatalf("keyless storage = %d/%d/%d, want 4/3/1", logs, receipts, keys)
	}
}

func TestNativeLogKeyExpiryAndPrune(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	key := "native-expiry-logs-0001"
	body := `[{"body":"expiring"}]`
	first := decodeNativeLogResult(t, sendNativeLogs(s, p, key, body))
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE native_log_acceptances SET expires_at=now()-interval '1 second'
		WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID); err != nil {
		t.Fatal(err)
	}
	pruned, err := s.pruneNativeLogAcceptances(ctx)
	if err != nil || pruned != 1 {
		t.Fatalf("expired retry key prune = %d %v, want 1", pruned, err)
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, p); logs != 1 || receipts != 1 || keys != 0 {
		t.Fatalf("prune touched logs or receipt: %d/%d/%d", logs, receipts, keys)
	}
	second := decodeNativeLogResult(t, sendNativeLogs(s, p, key, body))
	if second.ReceiptID == first.ReceiptID {
		t.Fatal("expired key replayed outside its retention window")
	}
	if _, err := pool.Exec(ctx, `UPDATE native_log_acceptances SET expires_at=now()-interval '1 second'
		WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID); err != nil {
		t.Fatal(err)
	}
	third := decodeNativeLogResult(t, sendNativeLogs(s, p, key, body))
	if third.ReceiptID == second.ReceiptID {
		t.Fatal("expired key replayed while the periodic sweep was pending")
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, p); logs != 3 || receipts != 3 || keys != 1 {
		t.Fatalf("post-expiry storage: %d/%d/%d, want 3/3/1", logs, receipts, keys)
	}
}

func TestNativeLogReplayKeepsOriginalDroppedCount(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := `[{"body":"stored"},{"body":"capped"}]`
	key := "native-capped-logs-0001"
	req := httptest.NewRequest(http.MethodPost, "/api/"+p.ID+"/logs", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	keyHash, err := nativeIngestKeyHash(req)
	if err != nil {
		t.Fatal(err)
	}
	records, err := ingest.ParseNativeLogs([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	receipt := newIngestReceipt(nil, req, p, "logs", "native")
	receipt.setReceived(len(records))
	budget := newIngestBudget()
	budget.left = 1 // Exercise the same row cap with a two-row fixture.
	first, replay, err := s.acceptNativeLogBatch(context.Background(), receipt, p, keyHash, nativeIngestPayloadHash([]byte(body)), records, budget)
	if err != nil || replay || first.Accepted != 1 || first.Rejected != 1 || first.Dropped != 1 {
		t.Fatalf("capped acceptance = %+v replay=%t err=%v", first, replay, err)
	}
	response := sendNativeLogs(s, p, key, body)
	again := decodeNativeLogResult(t, response)
	if response.Header().Get("Idempotency-Replayed") != "true" || again.Accepted != 1 || again.Rejected != 1 || again.Dropped != 1 || again.ReceiptID != first.ReceiptID {
		t.Fatalf("capped replay = %+v header=%q", again, response.Header().Get("Idempotency-Replayed"))
	}
	if logs, receipts, keys := countNativeLogRows(t, pool, p); logs != 1 || receipts != 1 || keys != 1 {
		t.Fatalf("capped storage = %d/%d/%d, want 1/1/1", logs, receipts, keys)
	}
}
