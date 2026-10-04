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
)

// These tests use a migrated throwaway PostgreSQL database when
// FLARE_TEST_DATABASE_URL is set. They prove the handler's wire response,
// tenant boundary, unique-key race, and metric/receipt commit as one unit.
func sendNativeMetrics(s *Server, p *generated.Project, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/"+p.ID+"/metrics", strings.NewReader(body))
	req.Header.Set("X-Flare-Key", p.PublicKey)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", p.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.handleNativeMetrics(rec, req)
	return rec
}

func TestNativeMetricKeyValidation(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		valid  bool
	}{
		{name: "legacy absent", valid: true},
		{name: "sender random hex", values: []string{"9f2a2fbe41dc1a792a874e188687a4fe"}, valid: true},
		{name: "short", values: []string{"short"}},
		{name: "empty", values: []string{""}},
		{name: "two values", values: []string{"9f2a2fbe41dc1a792a874e188687a4fe", "c6327a736cc548699d88caa26394e676"}},
		{name: "comma", values: []string{"9f2a2fbe41dc1a792a874e188687a4fe,c6327a736cc548699d88caa26394e676"}},
		{name: "space", values: []string{"9f2a2fbe41dc1a792a874e188687a4fe x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
			for _, v := range tc.values {
				req.Header.Add("Idempotency-Key", v)
			}
			hash, err := nativeMetricKeyHash(req)
			if (err == nil) != tc.valid {
				t.Fatalf("key hash=%q err=%v, valid=%t", hash, err, tc.valid)
			}
			if tc.valid && len(tc.values) == 1 && len(hash) != 64 {
				t.Fatalf("key hash length %d, want 64", len(hash))
			}
		})
	}
}

func decodeNativeMetricResult(t *testing.T, rec *httptest.ResponseRecorder) nativeMetricResult {
	t.Helper()
	var got nativeMetricResult
	if rec.Code != http.StatusOK {
		t.Fatalf("metric response = %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ReceiptID == "" {
		t.Fatalf("200 without durable receipt id: %s", rec.Body.String())
	}
	return got
}

func countNativeMetricRows(t *testing.T, pool *pgxpool.Pool, p *generated.Project) (points, receipts, keys int) {
	t.Helper()
	ctx := context.Background()
	for i, stmt := range []string{
		`SELECT count(*) FROM metrics WHERE org_id=$1 AND project_id=$2`,
		`SELECT count(*) FROM ingest_receipts WHERE org_id=$1 AND project_id=$2 AND pillar='metrics' AND protocol='native'`,
		`SELECT count(*) FROM native_metric_acceptances WHERE org_id=$1 AND project_id=$2`,
	} {
		var n int
		if err := pool.QueryRow(ctx, stmt, p.OrgID, p.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			points = n
		case 1:
			receipts = n
		case 2:
			keys = n
		}
	}
	return
}

func TestNativeMetricAtomicIdempotentAcceptance(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	// The second point has an oversized label document, so replay must keep
	// both the accepted and rejected counts from the original receipt.
	body := `[{"name":"checkout.requests","kind":"sum","value":1,"labels":{"route":"checkout"}},` +
		`{"name":"oversize","value":1,"labels":{"x":"` + strings.Repeat("x", maxMetricLabelBytes) + `"}}]`
	key := "native-checkout-batch-0001"
	const workers = 8
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- sendNativeMetrics(s, p, key, body)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var receiptID string
	newCount := 0
	for rec := range results {
		got := decodeNativeMetricResult(t, rec)
		if got.Accepted != 1 || got.Rejected != 1 || got.Dropped != 0 {
			t.Fatalf("metric counts = %+v", got)
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
	if points, receipts, keys := countNativeMetricRows(t, pool, p); points != 1 || receipts != 1 || keys != 1 {
		t.Fatalf("stored points/receipts/keys = %d/%d/%d, want 1/1/1", points, receipts, keys)
	}
	var received, accepted, rejected, dropped, persisted int64
	var outcome string
	var completed bool
	if err := pool.QueryRow(context.Background(), `SELECT received_count,accepted_count,rejected_count,dropped_count,persisted_count,
		outcome,completed_at IS NOT NULL FROM ingest_receipts WHERE id=$1 AND org_id=$2 AND project_id=$3`,
		receiptID, p.OrgID, p.ID).Scan(&received, &accepted, &rejected, &dropped, &persisted, &outcome, &completed); err != nil {
		t.Fatal(err)
	}
	if received != 2 || accepted != 1 || rejected != 1 || dropped != 0 || persisted != 1 || outcome != "partial" || !completed {
		t.Fatalf("terminal receipt = %d/%d/%d/%d/%d %s %t", received, accepted, rejected, dropped, persisted, outcome, completed)
	}
	changed := sendNativeMetrics(s, p, key, `[{"name":"checkout.requests","kind":"sum","value":2}]`)
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed bytes with same key = %d %s", changed.Code, changed.Body.String())
	}
	if points, receipts, keys := countNativeMetricRows(t, pool, p); points != 1 || receipts != 1 || keys != 1 {
		t.Fatalf("conflict changed storage: %d/%d/%d", points, receipts, keys)
	}

	other, err := generated.New(pool).CreateProject(context.Background(), generated.CreateProjectParams{
		ID: id.New(), OrgID: p.OrgID, Name: "same-org-other-project", Slug: "native-metric-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	second := decodeNativeMetricResult(t, sendNativeMetrics(s, other, key, body))
	if second.ReceiptID == receiptID {
		t.Fatal("replay key crossed project boundary")
	}
	if points, receipts, keys := countNativeMetricRows(t, pool, other); points != 1 || receipts != 1 || keys != 1 {
		t.Fatalf("other project storage: %d/%d/%d", points, receipts, keys)
	}
}

func TestNativeMetricAtomicRollbackAndKeylessCompatibility(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_native_metric() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.name = 'blocked' THEN RAISE EXCEPTION 'forced metric failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_native_metric BEFORE INSERT ON metrics FOR EACH ROW EXECUTE FUNCTION fail_native_metric()`); err != nil {
		t.Fatal(err)
	}
	body := `[{"name":"checkout.requests","value":1},{"name":"blocked","value":2}]`
	key := "native-rollback-batch-0001"
	failed := sendNativeMetrics(s, p, key, body)
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("Retry-After") != "1" {
		t.Fatalf("failed acceptance = %d %s", failed.Code, failed.Body.String())
	}
	if points, receipts, keys := countNativeMetricRows(t, pool, p); points != 0 || receipts != 0 || keys != 0 {
		t.Fatalf("failure left partial storage: %d/%d/%d", points, receipts, keys)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_native_metric ON metrics; DROP FUNCTION fail_native_metric()`); err != nil {
		t.Fatal(err)
	}
	// Fail the terminal receipt update after COPY has written its rows inside
	// the transaction. A 503 must still leave zero points, keys, and receipts.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_native_metric_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.pillar='metrics' AND NEW.protocol='native' THEN RAISE EXCEPTION 'forced receipt failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_native_metric_receipt BEFORE UPDATE ON ingest_receipts FOR EACH ROW EXECUTE FUNCTION fail_native_metric_receipt()`); err != nil {
		t.Fatal(err)
	}
	failed = sendNativeMetrics(s, p, key, body)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("receipt completion failure = %d %s", failed.Code, failed.Body.String())
	}
	if points, receipts, keys := countNativeMetricRows(t, pool, p); points != 0 || receipts != 0 || keys != 0 {
		t.Fatalf("receipt failure left partial storage: %d/%d/%d", points, receipts, keys)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_native_metric_receipt ON ingest_receipts; DROP FUNCTION fail_native_metric_receipt()`); err != nil {
		t.Fatal(err)
	}
	accepted := decodeNativeMetricResult(t, sendNativeMetrics(s, p, key, body))
	if accepted.Accepted != 2 || accepted.Rejected != 0 {
		t.Fatalf("retry counts = %+v", accepted)
	}
	// Keyless legacy senders still work and get terminal receipts before 200.
	keylessBody := `[{"name":"legacy","value":3}]`
	first := decodeNativeMetricResult(t, sendNativeMetrics(s, p, "", keylessBody))
	second := decodeNativeMetricResult(t, sendNativeMetrics(s, p, "", keylessBody))
	if first.ReceiptID == second.ReceiptID {
		t.Fatal("keyless calls unexpectedly shared a receipt")
	}
	if points, receipts, keys := countNativeMetricRows(t, pool, p); points != 4 || receipts != 3 || keys != 1 {
		t.Fatalf("keyless storage = %d/%d/%d, want 4/3/1", points, receipts, keys)
	}
}

func TestNativeMetricKeyExpiryAndPrune(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	key := "native-expiry-batch-0001"
	body := `[{"name":"expiring","value":1}]`
	first := decodeNativeMetricResult(t, sendNativeMetrics(s, p, key, body))
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE native_metric_acceptances SET expires_at=now()-interval '1 second'
		WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID); err != nil {
		t.Fatal(err)
	}
	pruned, err := s.pruneNativeMetricAcceptances(ctx)
	if err != nil || pruned != 1 {
		t.Fatalf("expired retry key prune = %d %v, want 1", pruned, err)
	}
	if points, receipts, keys := countNativeMetricRows(t, pool, p); points != 1 || receipts != 1 || keys != 0 {
		t.Fatalf("prune touched telemetry or receipt: %d/%d/%d", points, receipts, keys)
	}
	second := decodeNativeMetricResult(t, sendNativeMetrics(s, p, key, body))
	if second.ReceiptID == first.ReceiptID {
		t.Fatal("expired key replayed outside its retention window")
	}
	if points, receipts, keys := countNativeMetricRows(t, pool, p); points != 2 || receipts != 2 || keys != 1 {
		t.Fatalf("post-expiry storage = %d/%d/%d", points, receipts, keys)
	}
	// Reuse after expiry also works while the background sweep is behind:
	// the request removes only its own expired key under the transaction.
	if _, err := pool.Exec(ctx, `UPDATE native_metric_acceptances SET expires_at=now()-interval '1 second'
		WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID); err != nil {
		t.Fatal(err)
	}
	third := decodeNativeMetricResult(t, sendNativeMetrics(s, p, key, body))
	if third.ReceiptID == second.ReceiptID {
		t.Fatal("expired key replayed while background sweep was pending")
	}
	if points, receipts, keys := countNativeMetricRows(t, pool, p); points != 3 || receipts != 3 || keys != 1 {
		t.Fatalf("request-driven expiry storage = %d/%d/%d", points, receipts, keys)
	}
}
