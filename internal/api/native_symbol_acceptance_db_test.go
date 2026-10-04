package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
)

// These tests use a migrated disposable Postgres database when
// FLARE_TEST_DATABASE_URL is set. They exercise the real handler, quota lock,
// artifact write, and terminal receipt at the HTTP acceptance boundary.
func sendNativeSymbolArtifact(s *Server, p *generated.Project, name string, content []byte) *httptest.ResponseRecorder {
	body, _ := json.Marshal(nativeSymbolUploadRequest{
		Release: "mobile-42", Platform: "flutter", Format: "dart_symbols",
		Name: name, Content: base64.StdEncoding.EncodeToString(content),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+p.ID+"/symbol-artifacts", bytes.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", p.ID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, p.OrgID)
	rec := httptest.NewRecorder()
	s.handleUploadNativeSymbolArtifact(rec, req.WithContext(ctx))
	return rec
}

func countNativeSymbolAcceptance(t *testing.T, pool *pgxpool.Pool, p *generated.Project) (artifacts, receipts, accepted, rejected int) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM native_symbol_artifacts WHERE org_id=$1 AND project_id=$2`, p.OrgID, p.ID).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE outcome='accepted' AND http_status=201 AND accepted_count=1 AND persisted_count=1 AND completed_at IS NOT NULL),
		count(*) FILTER (WHERE outcome='rejected' AND http_status=507 AND rejected_count=1 AND completed_at IS NOT NULL)
		FROM ingest_receipts WHERE org_id=$1 AND project_id=$2 AND pillar='symbols' AND protocol='native'`,
		p.OrgID, p.ID).Scan(&receipts, &accepted, &rejected); err != nil {
		t.Fatal(err)
	}
	return
}

func TestNativeSymbolArtifactAndTerminalReceiptCommitTogether(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	first := sendNativeSymbolArtifact(s, p, "app.symbols", []byte("symbols-v1"))
	if first.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s", first.Code, first.Body.String())
	}
	var uploaded nativeSymbolArtifactResponse
	if err := json.Unmarshal(first.Body.Bytes(), &uploaded); err != nil || uploaded.ID == "" || uploaded.ReceiptID == "" || uploaded.Size != int64(len("symbols-v1")) {
		t.Fatalf("201 lacks artifact/receipt identity: %+v err=%v", uploaded, err)
	}
	if bytes.Contains(first.Body.Bytes(), []byte("symbols-v1")) {
		t.Fatal("upload response exposed private symbols")
	}
	if artifacts, receipts, accepted, rejected := countNativeSymbolAcceptance(t, pool, p); artifacts != 1 || receipts != 1 || accepted != 1 || rejected != 0 {
		t.Fatalf("artifact/receipt state = %d/%d/%d/%d, want 1/1/1/0", artifacts, receipts, accepted, rejected)
	}
	var releases int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM releases WHERE org_id=$1 AND project_id=$2 AND version='mobile-42'`, p.OrgID, p.ID).Scan(&releases); err != nil || releases != 1 {
		t.Fatalf("accepted artifact release marker = %d, err=%v", releases, err)
	}
	var terminal bool
	if err := pool.QueryRow(context.Background(), `SELECT received_count=1 AND accepted_count=1 AND persisted_count=1 AND
		rejected_count=0 AND dropped_count=0 AND outcome='accepted' AND http_status=201 AND NOT retryable AND completed_at IS NOT NULL
		FROM ingest_receipts WHERE id=$1 AND org_id=$2 AND project_id=$3`, uploaded.ReceiptID, p.OrgID, p.ID).Scan(&terminal); err != nil || !terminal {
		t.Fatalf("receipt %q was not terminal and tenant-scoped: %v", uploaded.ReceiptID, err)
	}
	second := sendNativeSymbolArtifact(s, p, "app.symbols", []byte("symbols-v2-longer"))
	if second.Code != http.StatusCreated {
		t.Fatalf("replacement = %d %s", second.Code, second.Body.String())
	}
	var replaced nativeSymbolArtifactResponse
	if err := json.Unmarshal(second.Body.Bytes(), &replaced); err != nil || replaced.ID != uploaded.ID || replaced.ReceiptID == uploaded.ReceiptID {
		t.Fatalf("replacement lost upsert/receipt semantics: %+v err=%v", replaced, err)
	}
	if artifacts, receipts, accepted, rejected := countNativeSymbolAcceptance(t, pool, p); artifacts != 1 || receipts != 2 || accepted != 2 || rejected != 0 {
		t.Fatalf("replacement state = %d/%d/%d/%d, want 1/2/2/0", artifacts, receipts, accepted, rejected)
	}
}

func TestNativeSymbolReceiptInsertFailureRollsBackArtifact(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_native_symbol_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.pillar='symbols' AND NEW.protocol='native' THEN RAISE EXCEPTION 'forced receipt failure'; END IF;
		RETURN NEW; END $$;
		CREATE TRIGGER reject_native_symbol_receipt BEFORE INSERT ON ingest_receipts
		FOR EACH ROW EXECUTE FUNCTION reject_native_symbol_receipt()`); err != nil {
		t.Fatal(err)
	}
	failed := sendNativeSymbolArtifact(s, p, "app.symbols", []byte("private-symbols"))
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("Retry-After") != "1" {
		t.Fatalf("receipt failure = %d %s", failed.Code, failed.Body.String())
	}
	if artifacts, receipts, accepted, rejected := countNativeSymbolAcceptance(t, pool, p); artifacts != 0 || receipts != 0 || accepted != 0 || rejected != 0 {
		t.Fatalf("failed receipt left artifact or success: %d/%d/%d/%d", artifacts, receipts, accepted, rejected)
	}
	var releases int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM releases WHERE org_id=$1 AND project_id=$2 AND version='mobile-42'`, p.OrgID, p.ID).Scan(&releases); err != nil || releases != 0 {
		t.Fatalf("failed artifact left a release marker: %d, err=%v", releases, err)
	}
}

func TestNativeSymbolConcurrentUploadsRespectProjectQuota(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	// Seed metadata-sized rows in one statement, then force eight distinct
	// uploads to race for the last permitted row on one project.
	if _, err := pool.Exec(context.Background(), `INSERT INTO native_symbol_artifacts
		(id,project_id,org_id,release,platform,format,name,digest,size,content,validation_status)
		SELECT 'seed-'||g,$1,$2,'baseline','android','proguard','map-'||g,repeat('0',64),1,decode('61','hex'),'not_checked'
		FROM generate_series(1,$3) AS g`, p.ID, p.OrgID, maxNativeSymbolArtifacts-1); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- sendNativeSymbolArtifact(s, p, fmt.Sprintf("app-%d.symbols", i), []byte("symbols"))
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	created, full := 0, 0
	var acceptedName string
	for rec := range results {
		switch rec.Code {
		case http.StatusCreated:
			created++
			var row nativeSymbolArtifactResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil || row.ReceiptID == "" {
				t.Fatalf("201 lacks committed receipt: %s err=%v", rec.Body.String(), err)
			}
			acceptedName = row.Name
		case http.StatusInsufficientStorage:
			full++
		default:
			t.Fatalf("racing upload = %d %s", rec.Code, rec.Body.String())
		}
	}
	if created != 1 || full != workers-1 {
		t.Fatalf("concurrent quota result = %d created, %d full", created, full)
	}
	if artifacts, receipts, accepted, rejected := countNativeSymbolAcceptance(t, pool, p); artifacts != maxNativeSymbolArtifacts || receipts != workers || accepted != 1 || rejected != workers-1 {
		t.Fatalf("quota state = %d/%d/%d/%d", artifacts, receipts, accepted, rejected)
	}
	// The count cap must still permit an overwrite of the same key.
	replacement := sendNativeSymbolArtifact(s, p, acceptedName, []byte("symbols-expanded"))
	if replacement.Code != http.StatusCreated {
		t.Fatalf("replacement at count cap = %d %s", replacement.Code, replacement.Body.String())
	}
	if artifacts, _, accepted, _ := countNativeSymbolAcceptance(t, pool, p); artifacts != maxNativeSymbolArtifacts || accepted != 2 {
		t.Fatalf("replacement changed count or lacked receipt: %d artifacts, %d accepted", artifacts, accepted)
	}
}

func TestNativeSymbolReplacementAccountsForByteDelta(t *testing.T) {
	pool := newTestDB(t)
	p := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	ctx := context.Background()
	// Synthetic size metadata reaches the 2 GiB boundary without allocating
	// two gigabytes of test blobs. The upload API itself always sets size from
	// the decoded content; only this fixture writes rows directly.
	if _, err := pool.Exec(ctx, `INSERT INTO native_symbol_artifacts
		(id,project_id,org_id,release,platform,format,name,digest,size,content,validation_status)
		SELECT 'large-'||g,$1,$2,'baseline','android','proguard','map-'||g,repeat('0',64),$3,decode('61','hex'),'not_checked'
		FROM generate_series(1,68) AS g`, p.ID, p.OrgID, maxNativeSymbolBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO native_symbol_artifacts
		(id,project_id,org_id,release,platform,format,name,digest,size,content,validation_status)
		VALUES ('remainder',$1,$2,'baseline','android','proguard','remainder',repeat('0',64),$3,decode('61','hex'),'not_checked'),
		       ('replace-me',$1,$2,'mobile-42','flutter','dart_symbols','app.symbols',repeat('0',64),1,decode('61','hex'),'not_checked')`,
		p.ID, p.OrgID, (8<<20)-2); err != nil {
		t.Fatal(err)
	}
	accepted := sendNativeSymbolArtifact(s, p, "app.symbols", []byte("ab"))
	if accepted.Code != http.StatusCreated {
		t.Fatalf("overwrite at byte cap = %d %s", accepted.Code, accepted.Body.String())
	}
	tooLarge := sendNativeSymbolArtifact(s, p, "app.symbols", []byte("abc"))
	if tooLarge.Code != http.StatusInsufficientStorage {
		t.Fatalf("growing overwrite over byte cap = %d %s", tooLarge.Code, tooLarge.Body.String())
	}
	newArtifact := sendNativeSymbolArtifact(s, p, "other.symbols", []byte("z"))
	if newArtifact.Code != http.StatusInsufficientStorage {
		t.Fatalf("new row over byte cap = %d %s", newArtifact.Code, newArtifact.Body.String())
	}
	var size int64
	var content []byte
	if err := pool.QueryRow(ctx, `SELECT size,content FROM native_symbol_artifacts
		WHERE org_id=$1 AND project_id=$2 AND release='mobile-42' AND name='app.symbols'`, p.OrgID, p.ID).Scan(&size, &content); err != nil || size != 2 || string(content) != "ab" {
		t.Fatalf("rejected overwrite changed retained bytes: size=%d content=%q err=%v", size, content, err)
	}
	if artifacts, receipts, acceptedCount, rejected := countNativeSymbolAcceptance(t, pool, p); artifacts != 70 || receipts != 3 || acceptedCount != 1 || rejected != 2 {
		t.Fatalf("byte quota state = %d/%d/%d/%d", artifacts, receipts, acceptedCount, rejected)
	}
}
