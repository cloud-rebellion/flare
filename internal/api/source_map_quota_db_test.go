package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// These tests use a migrated disposable Postgres database when
// FLARE_TEST_DATABASE_URL is set. The real upload handler must serialize
// admission across simultaneous requests and preserve tenant isolation.
const sourceMapQuotaFixture = `{"version":3,"sources":["app.ts"],"names":[],"mappings":"AAAA"}`

func sendSourceMapArtifact(s *Server, projectID, orgID, release, name, content string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{
		"release": release, "name": name, "content": content,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/artifacts", bytes.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", projectID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, orgID)
	rec := httptest.NewRecorder()
	s.handleUploadSourceMap(rec, req.WithContext(ctx))
	return rec
}

func sourceMapUsage(t *testing.T, pool *pgxpool.Pool, project *generated.Project) (int64, int64) {
	t.Helper()
	row, err := generated.New(pool).CountSourceMapsByProject(context.Background(), generated.CountSourceMapsByProjectParams{
		ProjectID: project.ID, OrgID: project.OrgID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row.Artifacts, row.Bytes
}

func TestSourceMapConcurrentUploadsAndReplacementAtCountCap(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(context.Background(), `INSERT INTO source_map_artifacts
		(id,project_id,org_id,release,name,content,parse_status)
		SELECT 'seed-'||g,$1,$2,'baseline','app-'||g,$3,'valid'
		FROM generate_series(1,$4) AS g`, project.ID, project.OrgID, sourceMapQuotaFixture, maxArtifactsPerProject-1); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- sendSourceMapArtifact(s, project.ID, project.OrgID, "web-42", fmt.Sprintf("app-%d.js", i), sourceMapQuotaFixture)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	created, full := 0, 0
	var accepted artifactResponse
	for rec := range results {
		switch rec.Code {
		case http.StatusCreated:
			created++
			if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
				t.Fatalf("decode accepted artifact: %v", err)
			}
		case http.StatusInsufficientStorage:
			full++
		default:
			t.Fatalf("racing upload = %d %s", rec.Code, rec.Body.String())
		}
	}
	if created != 1 || full != workers-1 {
		t.Fatalf("concurrent quota result = %d created, %d full", created, full)
	}
	if count, _ := sourceMapUsage(t, pool, project); count != maxArtifactsPerProject {
		t.Fatalf("concurrent upload crossed row cap: %d", count)
	}
	// At the exact row cap the same-key retry must remain possible and retain ID.
	retry := sendSourceMapArtifact(s, project.ID, project.OrgID, "web-42", accepted.Name, sourceMapQuotaFixture)
	if retry.Code != http.StatusCreated {
		t.Fatalf("same-key retry at count cap = %d %s", retry.Code, retry.Body.String())
	}
	var replaced artifactResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &replaced); err != nil || replaced.ID != accepted.ID {
		t.Fatalf("same-key retry changed identity: %+v err=%v", replaced, err)
	}
	if count, _ := sourceMapUsage(t, pool, project); count != maxArtifactsPerProject {
		t.Fatalf("same-key retry changed row count: %d", count)
	}
	// Admission must look up the normalized key used by the actual upsert.
	normalized := sendSourceMapArtifact(s, project.ID, project.OrgID, "web-42", accepted.Name+"\x00", sourceMapQuotaFixture)
	if normalized.Code != http.StatusCreated {
		t.Fatalf("normalized same-key retry at count cap = %d %s", normalized.Code, normalized.Body.String())
	}
	var normalizedRow artifactResponse
	if err := json.Unmarshal(normalized.Body.Bytes(), &normalizedRow); err != nil || normalizedRow.ID != accepted.ID {
		t.Fatalf("normalized retry changed identity: %+v err=%v", normalizedRow, err)
	}
}

func TestSourceMapReplacementAccountsForStoredByteDelta(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	ctx := context.Background()
	oldContent := sourceMapQuotaFixture
	newContent := `{"version":3,"sources":["app.ts"],"names":[],"mappings":"AAAA","file":"a.js"}`
	if _, err := pool.Exec(ctx, `INSERT INTO source_map_artifacts
		(id,project_id,org_id,release,name,content,parse_status)
		VALUES ($1,$2,$3,'web-42','app.js',$4,'valid')`, id.New(), project.ID, project.OrgID, oldContent); err != nil {
		t.Fatal(err)
	}
	artifact := generated.UpsertSourceMapParams{
		ID: id.New(), ProjectID: project.ID, OrgID: project.OrgID,
		Release: "web-42", Name: "app.js", Content: newContent, ParseStatus: "valid",
	}
	// A small test limit exercises the same transactional byte math as the
	// production 2 GiB limit without allocating a multi-gigabyte fixture.
	row, full, err := s.acceptSourceMapWithLimits(ctx, artifact, maxArtifactsPerProject, int64(len(newContent)))
	if err != nil || full || row == nil || int64(row.Size) != int64(len(newContent)) {
		t.Fatalf("replacement at exact byte cap = %+v full=%v err=%v", row, full, err)
	}
	artifact.Content = newContent + " "
	if _, full, err := s.acceptSourceMapWithLimits(ctx, artifact, maxArtifactsPerProject, int64(len(newContent))); err != nil || !full {
		t.Fatalf("growing replacement over byte cap: full=%v err=%v", full, err)
	}
	artifact.Name = "other.js"
	artifact.Content = sourceMapQuotaFixture
	if _, full, err := s.acceptSourceMapWithLimits(ctx, artifact, maxArtifactsPerProject, int64(len(newContent))); err != nil || !full {
		t.Fatalf("new row over byte cap: full=%v err=%v", full, err)
	}
	if count, used := sourceMapUsage(t, pool, project); count != 1 || used != int64(len(newContent)) {
		t.Fatalf("rejected writes changed retained quota: count=%d bytes=%d", count, used)
	}
}

func TestSourceMapUploadRejectsForeignTenantWithoutOverwrite(t *testing.T) {
	pool := newTestDB(t)
	victim := seedProjectForIngest(t, pool)
	attacker := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	first := sendSourceMapArtifact(s, victim.ID, victim.OrgID, "web-42", "app.js", sourceMapQuotaFixture)
	if first.Code != http.StatusCreated {
		t.Fatalf("initial upload = %d %s", first.Code, first.Body.String())
	}
	foreign := sendSourceMapArtifact(s, victim.ID, attacker.OrgID, "web-42", "app.js", sourceMapQuotaFixture+" ")
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign tenant overwrite = %d %s", foreign.Code, foreign.Body.String())
	}
	var stored string
	if err := pool.QueryRow(context.Background(), `SELECT content FROM source_map_artifacts
		WHERE project_id=$1 AND org_id=$2 AND release='web-42' AND name='app.js'`, victim.ID, victim.OrgID).Scan(&stored); err != nil || stored != sourceMapQuotaFixture {
		t.Fatalf("foreign upload changed victim map: content=%q err=%v", stored, err)
	}
	if count, _ := sourceMapUsage(t, pool, attacker); count != 0 {
		t.Fatalf("foreign upload created attacker artifact: %d", count)
	}
}
