package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/metrics"
)

// Exercise the member API-key boundary and real acceptance transaction. Symbol
// uploads must not pollute the bucket used for unsupported envelope item types.
func TestNativeSymbolIngestMetricsKeepCanonicalSignalAtHTTPBoundary(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	registry := metrics.New()
	previous := metrics.Default
	metrics.Default = registry
	t.Cleanup(func() { metrics.Default = previous })

	keys := make(map[string]string)
	for _, role := range []string{"viewer", "member"} {
		key := "synthetic-symbol-metrics-" + role
		if _, err := s.q.CreateAPIKey(context.Background(), generated.CreateAPIKeyParams{
			ID: id.New(), OrgID: project.OrgID, Name: "symbol metric regression",
			KeyHash: auth.HashAPIKey(key), KeyPrefix: "synthetic", Role: role,
		}); err != nil {
			t.Fatal(err)
		}
		keys[role] = key
	}
	router := chi.NewRouter()
	router.Use(s.requireAuth, s.requireRole("member"))
	router.Post("/api/projects/{id}/symbol-artifacts", s.handleUploadNativeSymbolArtifact)
	post := func(key, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/projects/"+project.ID+"/symbol-artifacts", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		registry.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	requireSeries := func(body string, want ...string) {
		t.Helper()
		for _, series := range want {
			if !strings.Contains(body, series+"\n") {
				t.Fatalf("missing exact counter %q:\n%s", series, body)
			}
		}
	}
	for _, denied := range []struct {
		key    string
		status int
	}{{"not-a-valid-key", http.StatusUnauthorized}, {keys["viewer"], http.StatusForbidden}} {
		if rec := post(denied.key, "{"); rec.Code != denied.status {
			t.Fatalf("unauthorized upload = %d, want %d", rec.Code, denied.status)
		}
	}
	if body := scrape(); strings.Contains(body, "flare_ingest_items_total{") || strings.Contains(body, "flare_ingest_requests_total{") {
		t.Fatalf("authentication refusal reached symbol ingest:\n%s", body)
	}

	if rec := post(keys["member"], "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed authenticated upload = %d, want 400", rec.Code)
	}
	body := scrape()
	requireSeries(body,
		`flare_ingest_items_total{outcome="invalid",signal="symbol"} 1`,
		`flare_ingest_requests_total{outcome="rejected",transport="native"} 1`,
	)
	if strings.Contains(body, `signal="other"`) {
		t.Fatalf("native symbol rejection polluted unsupported-item counters:\n%s", body)
	}

	payload, err := json.Marshal(nativeSymbolUploadRequest{
		Release: "mobile-42", Platform: "flutter", Format: "dart_symbols", Name: "app.symbols",
		Content: base64.StdEncoding.EncodeToString([]byte("synthetic-symbol-bytes")),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := post(keys["member"], string(payload))
	if accepted.Code != http.StatusCreated {
		t.Fatalf("accepted authenticated upload = %d %s", accepted.Code, accepted.Body.String())
	}
	var uploaded nativeSymbolArtifactResponse
	if err := json.Unmarshal(accepted.Body.Bytes(), &uploaded); err != nil || uploaded.ID == "" || uploaded.ReceiptID == "" {
		t.Fatalf("upload lacks artifact/receipt identity: %+v err=%v", uploaded, err)
	}
	if artifacts, receipts, stored, rejected := countNativeSymbolAcceptance(t, pool, project); artifacts != 1 || receipts != 1 || stored != 1 || rejected != 0 {
		t.Fatalf("accepted artifact/receipt state = %d/%d/%d/%d, want 1/1/1/0", artifacts, receipts, stored, rejected)
	}
	var terminal bool
	if err := pool.QueryRow(context.Background(), `SELECT received_count=1 AND accepted_count=1 AND persisted_count=1 AND
		rejected_count=0 AND dropped_count=0 AND outcome='accepted' AND http_status=201 AND NOT retryable AND completed_at IS NOT NULL
		FROM ingest_receipts WHERE id=$1 AND org_id=$2 AND project_id=$3`,
		uploaded.ReceiptID, project.OrgID, project.ID).Scan(&terminal); err != nil || !terminal {
		t.Fatalf("accepted upload receipt was not committed in scope: %v", err)
	}
	body = scrape()
	requireSeries(body,
		`flare_ingest_items_total{outcome="accepted",signal="symbol"} 1`,
		`flare_ingest_items_total{outcome="invalid",signal="symbol"} 1`,
		`flare_ingest_requests_total{outcome="accepted",transport="native"} 1`,
		`flare_ingest_requests_total{outcome="rejected",transport="native"} 1`,
	)
	if strings.Contains(body, `signal="other"`) {
		t.Fatalf("accepted native symbols polluted unsupported-item counters:\n%s", body)
	}

	// Positive control: an unsupported, DSN-authenticated envelope item must
	// still be visible as other.invalid, independently of native symbol traffic.
	unsupported := sendEnvelope(s, project.PublicKey, "{}\n{\"type\":\"session\"}\n{}\n")
	if unsupported.Code != http.StatusBadRequest {
		t.Fatalf("unsupported envelope = %d, want 400", unsupported.Code)
	}
	requireSeries(scrape(),
		`flare_ingest_items_total{outcome="invalid",signal="other"} 1`,
		`flare_ingest_requests_total{outcome="rejected",transport="sentry"} 1`,
		`flare_ingest_items_total{outcome="accepted",signal="symbol"} 1`,
		`flare_ingest_items_total{outcome="invalid",signal="symbol"} 1`,
		`flare_ingest_requests_total{outcome="rejected",transport="native"} 1`,
	)
}
