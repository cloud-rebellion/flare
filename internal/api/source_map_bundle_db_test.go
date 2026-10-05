package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func sendSourceMapBundle(s *Server, projectID, orgID string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/artifact-bundles", bytes.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", projectID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, orgID)
	rec := httptest.NewRecorder()
	s.handleUploadSourceMapBundle(rec, req.WithContext(ctx))
	return rec
}

func getSourceMapBundle(s *Server, projectID, orgID, bundleID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/artifact-bundles/"+bundleID, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", projectID)
	route.URLParams.Add("bundleID", bundleID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, orgID)
	rec := httptest.NewRecorder()
	s.handleGetSourceMapBundle(rec, req.WithContext(ctx))
	return rec
}

func deleteSourceMapArtifact(s *Server, projectID, orgID, artifactID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID+"/artifacts/"+artifactID, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", projectID)
	route.URLParams.Add("artifactID", artifactID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, orgID)
	rec := httptest.NewRecorder()
	s.handleDeleteSourceMap(rec, req.WithContext(ctx))
	return rec
}

func marshalMapBundle(t *testing.T, release, digest string, named map[string]string) []byte {
	t.Helper()
	artifacts := make([]map[string]string, 0, len(named))
	for name, content := range named {
		artifacts = append(artifacts, map[string]string{"name": name, "content": content})
	}
	body, err := json.Marshal(map[string]any{"release": release, "digest": digest, "artifacts": artifacts})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func decodeMapBundle(t *testing.T, rec *httptest.ResponseRecorder) sourceMapBundleReceipt {
	t.Helper()
	var got sourceMapBundleReceipt
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode bundle response %d: %v", rec.Code, err)
	}
	return got
}

func countMapBundles(t *testing.T, pool *pgxpool.Pool, project *generated.Project, release string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM source_map_bundles WHERE project_id=$1 AND org_id=$2 AND release=$3`, project.ID, project.OrgID, release).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSourceMapBundleQuotaIsAtomicAndReplayWorksAtCap(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO source_map_artifacts (id,project_id,org_id,release,name,content,parse_status)
		SELECT 'bundle-seed-'||g,$1,$2,'old','map-'||g,$3,'valid' FROM generate_series(1,$4) AS g`, project.ID, project.OrgID, sourceMapQuotaFixture, maxArtifactsPerProject-1); err != nil {
		t.Fatal(err)
	}
	two := marshalMapBundle(t, "web-42", "", map[string]string{"one.js": sourceMapQuotaFixture, "two.js": sourceMapQuotaFixture})
	rejected := sendSourceMapBundle(s, project.ID, project.OrgID, two)
	if rejected.Code != http.StatusInsufficientStorage {
		t.Fatalf("2-row bundle at 1999/2000 cap = %d %s", rejected.Code, rejected.Body.String())
	}
	if count, _ := sourceMapUsage(t, pool, project); count != maxArtifactsPerProject-1 || countMapBundles(t, pool, project, "web-42") != 0 {
		t.Fatalf("rejected bundle left partial rows or receipt: rows=%d", count)
	}
	one := marshalMapBundle(t, "web-42", "", map[string]string{"one.js": sourceMapQuotaFixture})
	created := sendSourceMapBundle(s, project.ID, project.OrgID, one)
	if created.Code != http.StatusCreated {
		t.Fatalf("one-row bundle at cap = %d %s", created.Code, created.Body.String())
	}
	receipt := decodeMapBundle(t, created)
	if receipt.ID == "" || receipt.ProjectID != project.ID || len(receipt.Artifacts) != 1 || receipt.Digest == "" || receipt.Artifacts[0].ParseStatus != "valid" || receipt.Artifacts[0].ContentSHA256 == "" {
		t.Fatalf("incomplete accepted bundle receipt: %+v", receipt)
	}
	if count, _ := sourceMapUsage(t, pool, project); count != maxArtifactsPerProject {
		t.Fatalf("accepted bundle did not reach exact cap: %d", count)
	}
	// A byte-for-byte retry stays possible at the cap and retains both receipt
	// and artifact identity, with no created_at update or quota delta.
	replayed := sendSourceMapBundle(s, project.ID, project.OrgID, one)
	if replayed.Code != http.StatusOK {
		t.Fatalf("exact replay at cap = %d %s", replayed.Code, replayed.Body.String())
	}
	replayReceipt := decodeMapBundle(t, replayed)
	if replayReceipt.ID != receipt.ID || replayReceipt.Digest != receipt.Digest || replayReceipt.CreatedAt != receipt.CreatedAt ||
		replayReceipt.Artifacts[0].ID != receipt.Artifacts[0].ID || replayReceipt.Artifacts[0].CreatedAt != receipt.Artifacts[0].CreatedAt || replayReceipt.Artifacts[0].ContentSHA256 != receipt.Artifacts[0].ContentSHA256 {
		t.Fatalf("replay changed durable receipt: first=%+v replay=%+v", receipt, replayReceipt)
	}
	if got := getSourceMapBundle(s, project.ID, project.OrgID, receipt.ID); got.Code != http.StatusOK || decodeMapBundle(t, got).Digest != receipt.Digest {
		t.Fatalf("bundle readback failed: %d %s", got.Code, got.Body.String())
	}
	wrongDigest := marshalMapBundle(t, "web-42", "sha256:"+hex.EncodeToString(sha256.New().Sum(nil)), map[string]string{"one.js": sourceMapQuotaFixture})
	if rec := sendSourceMapBundle(s, project.ID, project.OrgID, wrongDigest); rec.Code != http.StatusConflict {
		t.Fatalf("changed digest assertion = %d %s", rec.Code, rec.Body.String())
	}
	changed := marshalMapBundle(t, "web-42", "", map[string]string{"one.js": sourceMapQuotaFixture + " "})
	if rec := sendSourceMapBundle(s, project.ID, project.OrgID, changed); rec.Code != http.StatusConflict {
		t.Fatalf("changed bundle at cap = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSourceMapBundleTenantAndLegacyWriteBoundaries(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	attacker := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := marshalMapBundle(t, "web-43", "", map[string]string{"one.js": sourceMapQuotaFixture, "two.js": sourceMapQuotaFixture})
	if rec := sendSourceMapBundle(s, project.ID, attacker.OrgID, body); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign bundle upload = %d %s", rec.Code, rec.Body.String())
	}
	created := sendSourceMapBundle(s, project.ID, project.OrgID, body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create bundle = %d %s", created.Code, created.Body.String())
	}
	receipt := decodeMapBundle(t, created)
	if rec := getSourceMapBundle(s, project.ID, attacker.OrgID, receipt.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign bundle readback = %d %s", rec.Code, rec.Body.String())
	}
	if rec := sendSourceMapArtifact(s, project.ID, project.OrgID, "web-43", "one.js", sourceMapQuotaFixture+" "); rec.Code != http.StatusConflict {
		t.Fatalf("legacy single overwrite of immutable map = %d %s", rec.Code, rec.Body.String())
	}
	if rec := sendSourceMapArtifact(s, project.ID, project.OrgID, "web-43", "new.js", sourceMapQuotaFixture); rec.Code != http.StatusConflict {
		t.Fatalf("legacy single extension of immutable release = %d %s", rec.Code, rec.Body.String())
	}
	if rec := deleteSourceMapArtifact(s, project.ID, project.OrgID, receipt.Artifacts[0].ID); rec.Code != http.StatusConflict {
		t.Fatalf("legacy delete of immutable map = %d %s", rec.Code, rec.Body.String())
	}
	// The FK also prevents a direct delete from orphaning the durable receipt.
	if _, err := pool.Exec(context.Background(), `DELETE FROM source_map_artifacts WHERE id=$1`, receipt.Artifacts[0].ID); err == nil {
		t.Fatal("direct SQL delete orphaned a bundle item")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE source_map_artifacts SET content=$2 WHERE id=$1`, receipt.Artifacts[0].ID, sourceMapQuotaFixture+" "); err == nil {
		t.Fatal("direct SQL update falsified a bound bundle hash")
	}
	if count, _ := sourceMapUsage(t, pool, project); count != 2 || countMapBundles(t, pool, project, "web-43") != 1 {
		t.Fatalf("immutable release mutated: rows=%d", count)
	}
	// The legacy path remains available for unrelated, unbundled releases.
	single := sendSourceMapArtifact(s, project.ID, project.OrgID, "old", "old.js", sourceMapQuotaFixture)
	if single.Code != http.StatusCreated {
		t.Fatalf("legacy single unrelated release = %d %s", single.Code, single.Body.String())
	}
	var legacy artifactResponse
	if err := json.Unmarshal(single.Body.Bytes(), &legacy); err != nil {
		t.Fatal(err)
	}
	if rec := deleteSourceMapArtifact(s, project.ID, project.OrgID, legacy.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("legacy delete unrelated release = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSourceMapBundleAdmitsBrightCRMMapCount(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	// A configured BrightCRM build produced 233 private maps. Admit the full
	// release in one receipt so Stage never publishes a partially mapped image.
	named := make(map[string]string, 233)
	for i := 0; i < 233; i++ {
		named[fmt.Sprintf("chunk-%03d.js", i)] = sourceMapQuotaFixture
	}
	rec := sendSourceMapBundle(s, project.ID, project.OrgID, marshalMapBundle(t, "brightcrm-233", "", named))
	if rec.Code != http.StatusCreated {
		t.Fatalf("233-map release = %d %s", rec.Code, rec.Body.String())
	}
	receipt := decodeMapBundle(t, rec)
	if receipt.ArtifactCount != 233 || len(receipt.Artifacts) != 233 {
		t.Fatalf("partial 233-map receipt: count=%d artifacts=%d", receipt.ArtifactCount, len(receipt.Artifacts))
	}
	if count, _ := sourceMapUsage(t, pool, project); count != 233 {
		t.Fatalf("partial 233-map storage: %d", count)
	}
}

func TestSourceMapBundleAllowsProjectErasureCascade(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	created := sendSourceMapBundle(s, project.ID, project.OrgID, marshalMapBundle(t, "web-erase", "", map[string]string{"one.js": sourceMapQuotaFixture}))
	if created.Code != http.StatusCreated {
		t.Fatalf("create bundle = %d %s", created.Code, created.Body.String())
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM projects WHERE id=$1 AND org_id=$2`, project.ID, project.OrgID); err != nil {
		t.Fatalf("project erasure must remove bundle and map together: %v", err)
	}
	var left int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM source_map_bundles WHERE project_id=$1`, project.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("project erasure retained bundle: count=%d err=%v", left, err)
	}
}

func TestSourceMapBundleExactStoredByteQuota(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	content := sourceMapQuotaFixture
	sum := sha256.Sum256([]byte(content))
	items := []sourceMapBundleItem{
		{Name: "one.js", Content: content, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))},
		{Name: "two.js", Content: content, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))},
	}
	total := int64(2 * len(content))
	digest := sourceMapBundleDigest("web-44", items)
	if _, _, err := s.acceptSourceMapBundleWithLimits(context.Background(), project.ID, project.OrgID, "web-44", digest, items, total, 2000, total-1); !errors.Is(err, errSourceMapBundleQuota) {
		t.Fatalf("byte cap should reject whole bundle: %v", err)
	}
	if count, used := sourceMapUsage(t, pool, project); count != 0 || used != 0 || countMapBundles(t, pool, project, "web-44") != 0 {
		t.Fatalf("byte rejection changed rows: count=%d bytes=%d", count, used)
	}
	receipt, replay, err := s.acceptSourceMapBundleWithLimits(context.Background(), project.ID, project.OrgID, "web-44", digest, items, total, 2000, total)
	if err != nil || replay || receipt == nil || receipt.TotalBytes != total {
		t.Fatalf("exact normalized byte cap rejected: receipt=%+v replay=%v err=%v", receipt, replay, err)
	}
	if count, used := sourceMapUsage(t, pool, project); count != 2 || used != total {
		t.Fatalf("normalized byte quota mismatched storage: count=%d bytes=%d want=%d", count, used, total)
	}
}

func TestSourceMapBundleDigestVector(t *testing.T) {
	items := []sourceMapBundleItem{{Name: "one.js", Content: sourceMapQuotaFixture}}
	if got, want := sourceMapBundleDigest("web-42", items), "sha256:bcafca89a915667a7f88e7849843c75734ff1b478e68e51328b05b079dfec36d"; got != want {
		t.Fatalf("digest vector = %s, want %s", got, want)
	}
}

func TestSourceMapBundleRequestBounds(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	tooMany := make(map[string]string, maxSourceMapBundleItems+1)
	for i := 0; i < maxSourceMapBundleItems+1; i++ {
		tooMany["map-"+id.New()+".js"] = sourceMapQuotaFixture
	}
	if rec := sendSourceMapBundle(s, project.ID, project.OrgID, marshalMapBundle(t, "web-bounds", "", tooMany)); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d maps = %d %s", maxSourceMapBundleItems+1, rec.Code, rec.Body.String())
	}
	oversizedBody := []byte(strings.Repeat(" ", maxSourceMapBundleBody+1))
	if rec := sendSourceMapBundle(s, project.ID, project.OrgID, oversizedBody); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("64 MiB+1 body = %d %s", rec.Code, rec.Body.String())
	}
	if count, _ := sourceMapUsage(t, pool, project); count != 0 || countMapBundles(t, pool, project, "web-bounds") != 0 {
		t.Fatalf("out-of-bounds request wrote maps: rows=%d", count)
	}
}

func TestSourceMapBundleRejectsUnlistedExistingReleaseRows(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(context.Background(), `INSERT INTO source_map_artifacts (id,project_id,org_id,release,name,content,parse_status) VALUES ($1,$2,$3,'web-45','unlisted.js',$4,'valid')`, id.New(), project.ID, project.OrgID, sourceMapQuotaFixture); err != nil {
		t.Fatal(err)
	}
	rec := sendSourceMapBundle(s, project.ID, project.OrgID, marshalMapBundle(t, "web-45", "", map[string]string{"listed.js": sourceMapQuotaFixture}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("bundle omitted existing live map = %d %s", rec.Code, rec.Body.String())
	}
	if count, _ := sourceMapUsage(t, pool, project); count != 1 || countMapBundles(t, pool, project, "web-45") != 0 {
		t.Fatalf("conflict partially admitted bundle: rows=%d", count)
	}
}
