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

func sendClientConnectorAttempt(s *Server, p *generated.Project, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/"+p.DsnID+"/connector-attempts", strings.NewReader(body))
	req.Header.Set("X-Flare-Key", p.PublicKey)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", p.DsnID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.handleConnectorAttempt(rec, req)
	return rec
}

func decodeClientConnectorReceipt(t *testing.T, rec *httptest.ResponseRecorder) connectorAttemptResponse {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("connector response = %d %s", rec.Code, rec.Body.String())
	}
	var got connectorAttemptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Accepted || got.ReceiptID == "" || rec.Header().Get("X-Flare-Receipt-ID") != got.ReceiptID {
		t.Fatalf("202 without durable receipt in body and header: %s", rec.Body.String())
	}
	return got
}

func countClientConnectorRows(t *testing.T, pool *pgxpool.Pool, p *generated.Project) (attempts, receipts, keys int) {
	t.Helper()
	ctx := context.Background()
	for i, stmt := range []string{
		`SELECT count(*) FROM connector_attempts WHERE org_id=$1 AND project_id=$2 AND source_kind='client_reported'`,
		`SELECT count(*) FROM ingest_receipts WHERE org_id=$1 AND project_id=$2 AND pillar='connectors' AND protocol='native'`,
		`SELECT count(*) FROM client_connector_acceptances WHERE org_id=$1 AND project_id=$2`,
	} {
		var n int
		if err := pool.QueryRow(ctx, stmt, p.OrgID, p.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			attempts = n
		case 1:
			receipts = n
		case 2:
			keys = n
		}
	}
	return
}

const clientConnectorBody = `{"provider":"hubspot","operation":"contacts.upsert","channel":"http","outcome":"error","status_class":"5xx","elapsed_ms":842,"quota_remaining":4}`

func TestClientConnectorAtomicIdempotentAcceptance(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	key := "hubspot-app-attempt-0001"
	const workers = 8
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- sendClientConnectorAttempt(s, p, key, clientConnectorBody)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var receiptID string
	newCount := 0
	for rec := range results {
		got := decodeClientConnectorReceipt(t, rec)
		if receiptID == "" {
			receiptID = got.ReceiptID
		} else if got.ReceiptID != receiptID {
			t.Fatalf("retry returned %q, want original %q", got.ReceiptID, receiptID)
		}
		if rec.Header().Get("Idempotency-Replayed") != "true" {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("new attempts = %d, want 1", newCount)
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, p); attempts != 1 || receipts != 1 || keys != 1 {
		t.Fatalf("stored attempt/receipt/key = %d/%d/%d, want 1/1/1", attempts, receipts, keys)
	}
	var source string
	var connectionIDValid bool
	if err := pool.QueryRow(context.Background(), `SELECT source_kind,connection_id IS NOT NULL FROM connector_attempts
		WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID).Scan(&source, &connectionIDValid); err != nil {
		t.Fatal(err)
	}
	if source != "client_reported" || connectionIDValid {
		t.Fatalf("public attempt provenance = %s connection_bound=%t", source, connectionIDValid)
	}
	var received, accepted, persisted, status int64
	var outcome string
	var completed bool
	if err := pool.QueryRow(context.Background(), `SELECT received_count,accepted_count,persisted_count,http_status,
		outcome,completed_at IS NOT NULL FROM ingest_receipts WHERE id=$1 AND org_id=$2 AND project_id=$3`,
		receiptID, p.OrgID, p.ID).Scan(&received, &accepted, &persisted, &status, &outcome, &completed); err != nil {
		t.Fatal(err)
	}
	if received != 1 || accepted != 1 || persisted != 1 || status != 202 || outcome != "accepted" || !completed {
		t.Fatalf("terminal receipt = %d/%d/%d status=%d %s completed=%t", received, accepted, persisted, status, outcome, completed)
	}
	// Same normalized assertion with different serialized bytes is a conflict.
	changed := sendClientConnectorAttempt(s, p, key, ` {"provider":"hubspot","operation":"contacts.upsert","channel":"http","outcome":"error","status_class":"5xx","elapsed_ms":842,"quota_remaining":4}`)
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed bytes with same key = %d %s", changed.Code, changed.Body.String())
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, p); attempts != 1 || receipts != 1 || keys != 1 {
		t.Fatalf("conflict changed storage: %d/%d/%d", attempts, receipts, keys)
	}
	other, err := generated.New(pool).CreateProject(context.Background(), generated.CreateProjectParams{
		ID: id.New(), OrgID: p.OrgID, Name: "other", Slug: "connector-other-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	second := decodeClientConnectorReceipt(t, sendClientConnectorAttempt(s, other, key, clientConnectorBody))
	if second.ReceiptID == receiptID {
		t.Fatal("retry key crossed project boundary")
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, other); attempts != 1 || receipts != 1 || keys != 1 {
		t.Fatalf("other project storage: %d/%d/%d", attempts, receipts, keys)
	}
}

func TestClientConnectorAtomicRollbackAndKeylessCompatibility(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_client_connector() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.source_kind='client_reported' THEN RAISE EXCEPTION 'forced attempt failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_client_connector BEFORE INSERT ON connector_attempts FOR EACH ROW EXECUTE FUNCTION fail_client_connector()`); err != nil {
		t.Fatal(err)
	}
	key := "hubspot-rollback-attempt-0001"
	failed := sendClientConnectorAttempt(s, p, key, clientConnectorBody)
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("Retry-After") != "1" {
		t.Fatalf("attempt failure = %d %s", failed.Code, failed.Body.String())
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, p); attempts != 0 || receipts != 0 || keys != 0 {
		t.Fatalf("attempt failure left partial storage: %d/%d/%d", attempts, receipts, keys)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_client_connector ON connector_attempts; DROP FUNCTION fail_client_connector()`); err != nil {
		t.Fatal(err)
	}
	// A trigger that silently suppresses an INSERT must not let an :exec
	// query produce a false 202 or a receipt without a connector row.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION suppress_client_connector() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.source_kind='client_reported' THEN RETURN NULL; END IF; RETURN NEW; END $$;
		CREATE TRIGGER suppress_client_connector BEFORE INSERT ON connector_attempts FOR EACH ROW EXECUTE FUNCTION suppress_client_connector()`); err != nil {
		t.Fatal(err)
	}
	failed = sendClientConnectorAttempt(s, p, key, clientConnectorBody)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("suppressed attempt = %d %s", failed.Code, failed.Body.String())
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, p); attempts != 0 || receipts != 0 || keys != 0 {
		t.Fatalf("suppressed attempt left partial storage: %d/%d/%d", attempts, receipts, keys)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER suppress_client_connector ON connector_attempts; DROP FUNCTION suppress_client_connector()`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_client_connector_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.pillar='connectors' AND NEW.protocol='native' THEN RAISE EXCEPTION 'forced receipt failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_client_connector_receipt BEFORE UPDATE ON ingest_receipts FOR EACH ROW EXECUTE FUNCTION fail_client_connector_receipt()`); err != nil {
		t.Fatal(err)
	}
	failed = sendClientConnectorAttempt(s, p, key, clientConnectorBody)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("receipt failure = %d %s", failed.Code, failed.Body.String())
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, p); attempts != 0 || receipts != 0 || keys != 0 {
		t.Fatalf("receipt failure left partial storage: %d/%d/%d", attempts, receipts, keys)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_client_connector_receipt ON ingest_receipts; DROP FUNCTION fail_client_connector_receipt()`); err != nil {
		t.Fatal(err)
	}
	decodeClientConnectorReceipt(t, sendClientConnectorAttempt(s, p, key, clientConnectorBody))
	first := decodeClientConnectorReceipt(t, sendClientConnectorAttempt(s, p, "", clientConnectorBody))
	second := decodeClientConnectorReceipt(t, sendClientConnectorAttempt(s, p, "", clientConnectorBody))
	if first.ReceiptID == second.ReceiptID {
		t.Fatal("keyless attempts unexpectedly shared a receipt")
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, p); attempts != 3 || receipts != 3 || keys != 1 {
		t.Fatalf("keyless storage = %d/%d/%d, want 3/3/1", attempts, receipts, keys)
	}
}

func TestClientConnectorKeyExpiryAndPrune(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	key := "hubspot-expiry-attempt-0001"
	first := decodeClientConnectorReceipt(t, sendClientConnectorAttempt(s, p, key, clientConnectorBody))
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE client_connector_acceptances SET expires_at=now()-interval '1 second'
		WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID); err != nil {
		t.Fatal(err)
	}
	pruned, err := s.pruneClientConnectorAcceptances(ctx)
	if err != nil || pruned != 1 {
		t.Fatalf("expired retry key prune = %d %v, want 1", pruned, err)
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, p); attempts != 1 || receipts != 1 || keys != 0 {
		t.Fatalf("prune touched attempt or receipt: %d/%d/%d", attempts, receipts, keys)
	}
	second := decodeClientConnectorReceipt(t, sendClientConnectorAttempt(s, p, key, clientConnectorBody))
	if second.ReceiptID == first.ReceiptID {
		t.Fatal("expired key replayed after prune")
	}
	if _, err := pool.Exec(ctx, `UPDATE client_connector_acceptances SET expires_at=now()-interval '1 second'
		WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID); err != nil {
		t.Fatal(err)
	}
	third := decodeClientConnectorReceipt(t, sendClientConnectorAttempt(s, p, key, clientConnectorBody))
	if third.ReceiptID == second.ReceiptID {
		t.Fatal("expired key replayed before the background sweep")
	}
	if attempts, receipts, keys := countClientConnectorRows(t, pool, p); attempts != 3 || receipts != 3 || keys != 1 {
		t.Fatalf("post-expiry storage = %d/%d/%d, want 3/3/1", attempts, receipts, keys)
	}
}
