package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/metrics"
)

// FLARE_TEST_DATABASE_URL enables this end-to-end test against a freshly
// migrated PostgreSQL database. The handler, unique constraint, and receipt
// transaction are all exercised, including concurrent same-key arrivals.
func TestBusinessCheckIdempotentAcceptance(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	bearer, businessKeyID := seedBusinessKeyForTest(t, pool, project)
	s := serverForDB(pool)
	registry := metrics.New()
	previousRegistry := metrics.Default
	metrics.Default = registry
	t.Cleanup(func() { metrics.Default = previousRegistry })
	key := "business-operation-0001"
	body := `{"check_name":"checkout.completed","outcome":"ok","elapsed_ms":42}`

	const workers = 8
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- sendBusinessCheck(s, project, bearer, key, body)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var receiptID string
	for rec := range results {
		got := requireAcceptedBusinessCheck(t, rec)
		if receiptID == "" {
			receiptID = got
		} else if got != receiptID {
			t.Fatalf("concurrent retry created a second receipt: %q != %q", got, receiptID)
		}
	}
	countBusinessAcceptance(t, pool, project.ID, 1)
	assertBusinessAcceptedItems(t, registry, 1)

	// Simulate the caller losing the first 202 response: a normalized retry
	// must return the durable receipt instead of writing another observation.
	retry := sendBusinessCheck(s, project, bearer, key,
		`{"check_name":" Checkout.Completed ","outcome":"OK","elapsed_ms":42,"status_class":"5xx","error_code":"ignored"}`)
	if got := requireAcceptedBusinessCheck(t, retry); got != receiptID || retry.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("ambiguous-response retry = receipt %q replay %q, want %q/true", got, retry.Header().Get("Idempotency-Replayed"), receiptID)
	}
	countBusinessAcceptance(t, pool, project.ID, 1)
	assertBusinessAcceptedItems(t, registry, 1)

	conflict := sendBusinessCheck(s, project, bearer, key,
		`{"check_name":"checkout.completed","outcome":"error","elapsed_ms":42,"status_class":"5xx"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("changed payload with same key = %d %s, want 409", conflict.Code, conflict.Body.String())
	}
	countBusinessAcceptance(t, pool, project.ID, 1)

	missingElapsed := sendBusinessCheck(s, project, bearer, "business-operation-0002",
		`{"check_name":"checkout.completed","outcome":"ok"}`)
	if missingElapsed.Code != http.StatusBadRequest {
		t.Fatalf("missing elapsed_ms = %d %s, want 400", missingElapsed.Code, missingElapsed.Body.String())
	}
	countBusinessAcceptance(t, pool, project.ID, 1)
	assertBusinessKeyUsage(t, pool, project, businessKeyID, workers+3, 2, 0)

	// A different project can use the same opaque key without learning or
	// reusing this project's receipt.
	otherProject := seedProjectForIngest(t, pool)
	if wrongProject := sendBusinessCheck(s, otherProject, bearer, key, body); wrongProject.Code != http.StatusUnauthorized {
		t.Fatalf("business key accepted for another project: %d", wrongProject.Code)
	}
	otherBearer, _ := seedBusinessKeyForTest(t, pool, otherProject)
	other := sendBusinessCheck(s, otherProject, otherBearer, key, body)
	if got := requireAcceptedBusinessCheck(t, other); got == receiptID {
		t.Fatal("idempotency key leaked a receipt across projects")
	}
	countBusinessAcceptance(t, pool, otherProject.ID, 1)
	var storedKeyID string
	if err := pool.QueryRow(context.Background(), "SELECT business_key_id FROM business_checks WHERE project_id=$1", project.ID).Scan(&storedKeyID); err != nil || storedKeyID != businessKeyID {
		t.Fatalf("business provenance key = %q (%v), want %q", storedKeyID, err, businessKeyID)
	}
}

func assertBusinessAcceptedItems(t *testing.T, registry *metrics.Metrics, want int) {
	t.Helper()
	out := httptest.NewRecorder()
	registry.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	wantSeries := fmt.Sprintf(`flare_ingest_items_total{outcome="accepted",signal="business"} %d`, want)
	if !strings.Contains(out.Body.String(), wantSeries) {
		t.Fatalf("business accepted-item metric missing %q:\n%s", wantSeries, out.Body.String())
	}
}

func TestBusinessCheckAcceptanceTimesOutOnLockedKey(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	bearer, businessKeyID := seedBusinessKeyForTest(t, pool, project)
	s := serverForDB(pool)
	key := "business-locked-operation-0001"
	body := `{"check_name":"checkout.completed","outcome":"ok","elapsed_ms":42}`
	keyRequest := httptest.NewRequest(http.MethodPost, "/api/p/business-checks", nil)
	keyRequest.Header.Set("Idempotency-Key", key)
	keyHash, err := businessIdempotencyKeyHash(keyRequest)
	if err != nil {
		t.Fatal(err)
	}

	// Hold an uncommitted unique-key reservation. The real handler's insert
	// must wait, hit its acceptance deadline, and return a retryable 503.
	ctx := context.Background()
	locked, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(ctx)
	qtx := generated.New(locked)
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	receiptID := id.New()
	if _, err := qtx.InsertIngestReceipt(ctx, generated.InsertIngestReceiptParams{
		ID: receiptID, OrgID: project.OrgID, ProjectID: project.ID, Pillar: "business", Protocol: "native",
		Outcome: "in_progress", HttpStatus: http.StatusAccepted, Retryable: true, ReceivedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := qtx.InsertBusinessCheck(ctx, generated.InsertBusinessCheckParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, ReceiptID: receiptID, BusinessKeyID: businessKeyID,
		IdempotencyKeyHash: keyHash, PayloadHash: strings.Repeat("0", 64),
		CheckName: "checkout.completed", Outcome: "ok", StatusClass: "2xx", ElapsedMs: 42, CompletedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	results := make(chan *httptest.ResponseRecorder, 1)
	go func() { results <- sendBusinessCheck(s, project, bearer, key, body) }()
	select {
	case rec := <-results:
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("locked acceptance = %d %s, want retryable 503", rec.Code, rec.Body.String())
		}
	case <-time.After(9 * time.Second):
		_ = locked.Rollback(ctx)
		t.Fatal("locked acceptance exceeded its bounded transaction deadline")
	}
	if err := locked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	countBusinessAcceptance(t, pool, project.ID, 0)
	_ = requireAcceptedBusinessCheck(t, sendBusinessCheck(s, project, bearer, key, body))
	countBusinessAcceptance(t, pool, project.ID, 1)
	assertBusinessKeyUsage(t, pool, project, businessKeyID, 2, 1, 0)
}

func sendBusinessCheck(s *Server, project *generated.Project, bearer, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/"+project.DsnID+"/business-checks", strings.NewReader(body))
	req.Header.Set("X-Flare-Key", project.PublicKey)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Idempotency-Key", key)
	route := chi.NewRouteContext()
	route.URLParams.Add("projectID", project.DsnID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	rec := httptest.NewRecorder()
	s.handleBusinessCheck(rec, req)
	return rec
}

func seedBusinessKeyForTest(t *testing.T, pool *pgxpool.Pool, project *generated.Project) (string, string) {
	t.Helper()
	randomPart, err := id.Token(24)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := "flb_" + randomPart
	k, err := generated.New(pool).CreateBusinessCheckKey(context.Background(), generated.CreateBusinessCheckKeyParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, Name: "test backend",
		KeyHash: auth.HashAPIKey(plaintext), KeyPrefix: plaintext[:12],
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plaintext, k.ID
}

func requireAcceptedBusinessCheck(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("business check = %d %s, want 202", rec.Code, rec.Body.String())
	}
	var out businessCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Accepted || out.ReceiptID == "" {
		t.Fatalf("business response = %#v", out)
	}
	return out.ReceiptID
}

func countBusinessAcceptance(t *testing.T, pool *pgxpool.Pool, projectID string, want int) {
	t.Helper()
	var checks, receipts int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM business_checks WHERE project_id=$1", projectID).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM ingest_receipts WHERE project_id=$1 AND pillar='business' AND outcome='accepted'", projectID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if checks != want || receipts != want {
		t.Fatalf("durable acceptance = %d checks / %d receipts, want %d each", checks, receipts, want)
	}
}
