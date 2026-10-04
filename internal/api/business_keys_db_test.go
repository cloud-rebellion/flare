package api

import (
	"context"
	"encoding/json"
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
)

func TestBusinessKeyLifecycleRejectsPublicDSNAndRevokedKeys(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	otherProject := seedProjectForIngest(t, pool)
	s := serverForDB(pool)

	created := businessKeyAdminRequest(s, project, http.MethodPost, "", `{"name":"backend","expires_in_days":30}`)
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create business key = %d %s", created.Code, created.Body.String())
	}
	var minted struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &minted); err != nil || !businessBearerToken.MatchString(minted.Key) || minted.ID == "" {
		t.Fatalf("invalid one-time business key response: %v", err)
	}
	var storedHash, storedPrefix string
	if err := pool.QueryRow(context.Background(), "SELECT key_hash,key_prefix FROM business_check_keys WHERE id=$1", minted.ID).Scan(&storedHash, &storedPrefix); err != nil {
		t.Fatal(err)
	}
	if storedHash != auth.HashAPIKey(minted.Key) || storedHash == minted.Key || storedPrefix != minted.Key[:12] {
		t.Fatal("business key was not stored as a hash and bounded display prefix")
	}
	listed := businessKeyAdminRequest(s, project, http.MethodGet, "", "")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), minted.Key) || strings.Contains(listed.Body.String(), storedHash) {
		t.Fatalf("business key list exposed a secret or failed: %d %s", listed.Code, listed.Body.String())
	}
	var metadata []businessKeyMetadata
	if err := json.Unmarshal(listed.Body.Bytes(), &metadata); err != nil || len(metadata) != 1 || metadata[0].ID != minted.ID || metadata[0].Prefix != storedPrefix {
		t.Fatalf("business key list = %#v (%v)", metadata, err)
	}

	const body = `{"check_name":"checkout.completed","outcome":"ok","elapsed_ms":42}`
	const idempotency = "business-key-lifecycle-0001"
	noBearer := sendBusinessCheck(s, project, "", idempotency, body)
	if noBearer.Code != http.StatusUnauthorized {
		t.Fatalf("public DSN alone authorized business check: %d", noBearer.Code)
	}
	unknown := sendBusinessCheck(s, project, "flb_"+strings.Repeat("0", 48), idempotency, body)
	wrongProject := sendBusinessCheck(s, otherProject, minted.Key, idempotency, body)
	if unknown.Code != http.StatusUnauthorized || wrongProject.Code != http.StatusUnauthorized || unknown.Body.String() != wrongProject.Body.String() || unknown.Body.String() != noBearer.Body.String() {
		t.Fatalf("business key validity or project scope leaked: missing=%d unknown=%d wrong=%d", noBearer.Code, unknown.Code, wrongProject.Code)
	}
	accepted := sendBusinessCheck(s, project, minted.Key, idempotency, body)
	_ = requireAcceptedBusinessCheck(t, accepted)
	malformed := sendBusinessCheck(s, project, minted.Key, "business-key-lifecycle-invalid-0001", `{"check_name":"checkout.completed","outcome":"ok","elapsed_ms":-1}`)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed authenticated business check = %d %s", malformed.Code, malformed.Body.String())
	}
	var provenance string
	if err := pool.QueryRow(context.Background(), "SELECT business_key_id FROM business_checks WHERE project_id=$1", project.ID).Scan(&provenance); err != nil || provenance != minted.ID {
		t.Fatalf("business check provenance = %q (%v), want %q", provenance, err, minted.ID)
	}
	assertBusinessKeyUsage(t, pool, project, minted.ID, 2, 1, 0)
	q := generated.New(pool)
	usageArg := generated.GetUsageThresholdStatsParams{
		OrgID: project.OrgID, ProjectID: project.ID, Surface: "business_key", Operation: "business_checks",
		Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}
	usageStats, err := q.GetUsageThresholdStats(context.Background(), usageArg)
	if err != nil || usageStats.RequestCount != 2 {
		t.Fatalf("business key usage threshold count = %#v (%v)", usageStats, err)
	}
	usageArg.ProjectID = otherProject.ID
	otherStats, err := q.GetUsageThresholdStats(context.Background(), usageArg)
	if err != nil || otherStats.RequestCount != 0 {
		t.Fatalf("other project observed business key usage = %#v (%v)", otherStats, err)
	}
	revoked := businessKeyAdminRequest(s, project, http.MethodDelete, minted.ID, "")
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke business key = %d %s", revoked.Code, revoked.Body.String())
	}
	afterRevoke := sendBusinessCheck(s, project, minted.Key, "business-key-lifecycle-0002", body)
	if afterRevoke.Code != http.StatusUnauthorized || afterRevoke.Body.String() != noBearer.Body.String() {
		t.Fatalf("revoked business key accepted or leaked validity: %d %s", afterRevoke.Code, afterRevoke.Body.String())
	}
	assertBusinessKeyUsage(t, pool, project, minted.ID, 2, 1, 0)
	countBusinessAcceptance(t, pool, project.ID, 1)
	var auditCount int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_log WHERE org_id=$1 AND actor_user_id='test-human-operator' AND action IN ('business_key.create','business_key.revoke')", project.OrgID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("business key audit count = %d (%v), want 2", auditCount, err)
	}
}

func TestBusinessKeyRevocationWinsBeforeAcceptanceTransaction(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	bearer, keyID := seedBusinessKeyForTest(t, pool, project)
	s := serverForDB(pool)

	ctx := context.Background()
	revoking, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer revoking.Rollback(ctx)
	if _, err := revoking.Exec(ctx, "UPDATE business_check_keys SET revoked_at=now() WHERE id=$1", keyID); err != nil {
		t.Fatal(err)
	}

	results := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		results <- sendBusinessCheck(s, project, bearer, "business-revoke-race-0001",
			`{"check_name":"checkout.completed","outcome":"ok","elapsed_ms":42}`)
	}()
	// The first auth lookup sees the last committed active row, then the
	// transaction's FOR SHARE check waits for this revocation transaction.
	select {
	case rec := <-results:
		t.Fatalf("business acceptance completed before pending revocation: %d %s", rec.Code, rec.Body.String())
	case <-time.After(150 * time.Millisecond):
	}
	if err := revoking.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-results:
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("business check after revocation = %d %s, want 401", rec.Code, rec.Body.String())
		}
	case <-time.After(7 * time.Second):
		t.Fatal("business acceptance stayed blocked after revocation committed")
	}
	countBusinessAcceptance(t, pool, project.ID, 0)
	assertBusinessKeyUsage(t, pool, project, keyID, 1, 1, 1)
}

func TestBusinessKeyActiveCapSerializesConcurrentProvisioning(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	var firstID string
	for i := 0; i < maxActiveBusinessKeysPerProject-1; i++ {
		created := businessKeyAdminRequest(s, project, http.MethodPost, "", `{"name":"backend"}`)
		if created.Code != http.StatusCreated {
			t.Fatalf("seed key %d = %d %s", i, created.Code, created.Body.String())
		}
		if i == 0 {
			var key struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &key); err != nil || key.ID == "" {
				t.Fatalf("seed key id = %q (%v)", key.ID, err)
			}
			firstID = key.ID
		}
	}

	const contenders = 6
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- businessKeyAdminRequest(s, project, http.MethodPost, "", `{"name":"replacement"}`)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	createdCount, cappedCount := 0, 0
	for rec := range results {
		switch rec.Code {
		case http.StatusCreated:
			createdCount++
		case http.StatusConflict:
			cappedCount++
			if strings.Contains(rec.Body.String(), "flb_") {
				t.Fatal("cap rejection exposed a generated secret")
			}
		default:
			t.Fatalf("concurrent key creation = %d %s", rec.Code, rec.Body.String())
		}
	}
	if createdCount != 1 || cappedCount != contenders-1 {
		t.Fatalf("concurrent cap outcome = %d created, %d capped", createdCount, cappedCount)
	}
	var active int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM business_check_keys WHERE org_id=$1 AND project_id=$2 AND revoked_at IS NULL AND expires_at>now()", project.OrgID, project.ID).Scan(&active); err != nil || active != maxActiveBusinessKeysPerProject {
		t.Fatalf("active business key count = %d (%v)", active, err)
	}
	// The winning request created a replacement while the old key was still
	// usable. Revoking that old key frees a slot for the next rotation.
	if rec := businessKeyAdminRequest(s, project, http.MethodDelete, firstID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke old key = %d %s", rec.Code, rec.Body.String())
	}
	if rec := businessKeyAdminRequest(s, project, http.MethodPost, "", `{"name":"next replacement"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create key after revoke = %d %s", rec.Code, rec.Body.String())
	}
}

func TestBusinessKeyListKeepsOldActiveKeyVisibleAfterHistoryExceedsLimit(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	created := businessKeyAdminRequest(s, project, http.MethodPost, "", `{"name":"old active backend"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create old active key = %d %s", created.Code, created.Body.String())
	}
	var active struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &active); err != nil || active.ID == "" {
		t.Fatalf("active key id = %q (%v)", active.ID, err)
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO business_check_keys (id, org_id, project_id, name, key_hash, key_prefix, created_at, expires_at, revoked_at)
		SELECT 'historical-' || gs::text, $1, $2, 'retired backend', lpad(to_hex(gs), 64, '0'),
		       'flb_00000000', now() + gs * interval '1 second', now() + interval '30 days', now()
		FROM generate_series(1, 110) AS gs`, project.OrgID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	listed := businessKeyAdminRequest(s, project, http.MethodGet, "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list business keys = %d %s", listed.Code, listed.Body.String())
	}
	var metadata []businessKeyMetadata
	if err := json.Unmarshal(listed.Body.Bytes(), &metadata); err != nil || len(metadata) != 100 {
		t.Fatalf("bounded business key history list has %d rows (%v)", len(metadata), err)
	}
	if metadata[0].ID != active.ID {
		t.Fatalf("old active key hidden by revoked history: first=%q want=%q", metadata[0].ID, active.ID)
	}
}

func assertBusinessKeyUsage(t *testing.T, pool *pgxpool.Pool, project *generated.Project, keyID string, wantRequests, wantErrors, wantDenied int64) {
	t.Helper()
	var requests, errors, denied int64
	err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(request_count),0), COALESCE(sum(error_count),0), COALESCE(sum(denied_count),0)
		FROM usage_buckets WHERE org_id=$1 AND project_id=$2 AND credential_id=$3 AND surface='business_key' AND operation='business_checks'`,
		project.OrgID, project.ID, keyID).Scan(&requests, &errors, &denied)
	if err != nil || requests != wantRequests || errors != wantErrors || denied != wantDenied {
		t.Fatalf("business key usage = %d/%d/%d (%v), want %d/%d/%d", requests, errors, denied, err, wantRequests, wantErrors, wantDenied)
	}
}

func TestBusinessKeyMutationRollsBackWhenAuditUnavailable(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	bearer, keyID := seedBusinessKeyForTest(t, pool, project)
	s := serverForDB(pool)
	if _, err := pool.Exec(context.Background(), "DROP TABLE audit_log"); err != nil {
		t.Fatal(err)
	}
	created := businessKeyAdminRequest(s, project, http.MethodPost, "", `{"name":"unlogged backend"}`)
	if created.Code != http.StatusInternalServerError {
		t.Fatalf("key created without audit: %d %s", created.Code, created.Body.String())
	}
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM business_check_keys WHERE project_id=$1", project.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unaudited key persisted: count %d (%v), want only the seeded key", count, err)
	}
	revoked := businessKeyAdminRequest(s, project, http.MethodDelete, keyID, "")
	if revoked.Code != http.StatusInternalServerError {
		t.Fatalf("key revoked without audit: %d %s", revoked.Code, revoked.Body.String())
	}
	if got := sendBusinessCheck(s, project, bearer, "business-audit-rollback-0001",
		`{"check_name":"checkout.completed","outcome":"ok","elapsed_ms":42}`); got.Code != http.StatusAccepted {
		t.Fatalf("failed revocation changed key state: %d %s", got.Code, got.Body.String())
	}
}

func TestBusinessKeyManagementRequiresHumanActor(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+project.ID+"/business-keys", strings.NewReader(`{"name":"no actor"}`))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", project.ID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, project.OrgID)
	rec := httptest.NewRecorder()
	s.handleCreateBusinessCheckKey(rec, req.WithContext(ctx))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("business key created without a human actor: %d %s", rec.Code, rec.Body.String())
	}
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM business_check_keys WHERE project_id=$1", project.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthenticated business key rows = %d (%v)", count, err)
	}
}

func businessKeyAdminRequest(s *Server, project *generated.Project, method, keyID, body string) *httptest.ResponseRecorder {
	path := "/api/projects/" + project.ID + "/business-keys"
	if keyID != "" {
		path += "/" + keyID
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", project.ID)
	route.URLParams.Add("keyID", keyID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, project.OrgID)
	ctx = context.WithValue(ctx, ctxUserID, "test-human-operator")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		s.handleListBusinessCheckKeys(rec, req)
	case http.MethodPost:
		s.handleCreateBusinessCheckKey(rec, req)
	case http.MethodDelete:
		s.handleRevokeBusinessCheckKey(rec, req)
	}
	return rec
}
