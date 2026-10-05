package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

type testSourceMapRetirementFencer struct {
	proof    sourceMapReleaseProtection
	acquired int
	released int
}

func (f *testSourceMapRetirementFencer) Acquire(_ context.Context, _, _, _ string) (sourceMapReleaseProtection, error) {
	f.acquired++
	p := f.proof
	p.ReleaseFence = func() { f.released++ }
	return p, nil
}

func retirementProof(project *generated.Project) *testSourceMapRetirementFencer {
	return &testSourceMapRetirementFencer{proof: sourceMapReleaseProtection{
		OrgID: project.OrgID, ProjectID: project.ID, CandidateRelease: "web-old",
		ActiveReleases: []string{}, RollbackReleases: []string{},
		Ref: "stage-retirement-fence-1", Generation: "generation-1", CapturedAt: time.Now(),
		EventPolicyRef: "browser-lifetime-policy-1", LastValidEventAt: time.Now().UTC().AddDate(0, 0, -31),
	}}
}

func retireBundleRequest(s *Server, projectID, orgID, actor, bundleID, digest string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID+"/artifact-bundles/"+bundleID, nil)
	if digest != "" {
		req.Header.Set("If-Match", `"`+digest+`"`)
	}
	route := chi.NewRouteContext()
	route.URLParams.Add("id", projectID)
	route.URLParams.Add("bundleID", bundleID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, orgID)
	if actor != "" {
		ctx = context.WithValue(ctx, ctxUserID, actor)
		ctx = context.WithValue(ctx, ctxAuthSource, "session")
	}
	rec := httptest.NewRecorder()
	s.handleRetireSourceMapBundle(rec, req.WithContext(ctx))
	return rec
}

func getRetirementRequest(s *Server, projectID, orgID, retirementID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/artifact-bundle-retirements/"+retirementID, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", projectID)
	route.URLParams.Add("retirementID", retirementID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, orgID)
	rec := httptest.NewRecorder()
	s.handleGetSourceMapRetirement(rec, req.WithContext(ctx))
	return rec
}

func seedOldRetirementBundle(t *testing.T, pool *pgxpool.Pool, project *generated.Project, release string) sourceMapBundleReceipt {
	t.Helper()
	s := serverForDB(pool)
	rec := sendSourceMapBundle(s, project.ID, project.OrgID,
		marshalMapBundle(t, release, "", map[string]string{"app.js": sourceMapQuotaFixture}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create bundle: %d %s", rec.Code, rec.Body.String())
	}
	bundle := decodeMapBundle(t, rec)
	if _, err := pool.Exec(context.Background(), `UPDATE source_map_bundles SET created_at=now()-interval '31 days' WHERE id=$1`, bundle.ID); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func assertRetirementUnchanged(t *testing.T, pool *pgxpool.Pool, project *generated.Project, bundle sourceMapBundleReceipt) {
	t.Helper()
	if got := countMapBundles(t, pool, project, bundle.Release); got != 1 {
		t.Fatalf("bundle count after rejection = %d, want 1", got)
	}
	if count, _ := sourceMapUsage(t, pool, project); count != int64(bundle.ArtifactCount) {
		t.Fatalf("map count after rejection = %d, want %d", count, bundle.ArtifactCount)
	}
	var retired, audited int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM source_map_bundle_retirements WHERE bundle_id=$1`, bundle.ID).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE target=$1 AND action='sourcemap.bundle.retire'`, bundle.ID).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if retired != 0 || audited != 0 {
		t.Fatalf("rejection committed retirement=%d audit=%d", retired, audited)
	}
}

func TestSourceMapRetirementFailsClosedWithoutTrustedFence(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	bundle := seedOldRetirementBundle(t, pool, project, "web-old")
	s := serverForDB(pool)
	if rec := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, bundle.Digest); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing proof = %d %s", rec.Code, rec.Body.String())
	}
	assertRetirementUnchanged(t, pool, project, bundle)
	if rec := retireBundleRequest(s, project.ID, "foreign-org", "human-admin", bundle.ID, bundle.Digest); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign project = %d %s", rec.Code, rec.Body.String())
	}
	if rec := retireBundleRequest(s, project.ID, project.OrgID, "", bundle.ID, bundle.Digest); rec.Code != http.StatusForbidden {
		t.Fatalf("API key/no human = %d %s", rec.Code, rec.Body.String())
	}
	f := retirementProof(project)
	s.sourceMapRetirementFencer = f
	f.proof.EventPolicyRef = ""
	if rec := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, bundle.Digest); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing bounded-lateness policy = %d %s", rec.Code, rec.Body.String())
	}
	f.proof.EventPolicyRef = "browser-lifetime-policy-1"
	f.proof.LastValidEventAt = time.Now().UTC().AddDate(0, 0, -20)
	if rec := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, bundle.Digest); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpired old-client horizon = %d %s", rec.Code, rec.Body.String())
	}
	f.proof.LastValidEventAt = time.Now().UTC().AddDate(0, 0, -31)
	s.cfg.RetentionDays = 14
	if rec := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, bundle.Digest); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("short hot-event retention = %d %s", rec.Code, rec.Body.String())
	}
	assertRetirementUnchanged(t, pool, project, bundle)
}

func TestSourceMapRetirementProtectsLiveRollbackAndRecentReleases(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*testing.T, *pgxpool.Pool, *generated.Project, *testSourceMapRetirementFencer)
	}{
		{"live", func(_ *testing.T, _ *pgxpool.Pool, _ *generated.Project, f *testSourceMapRetirementFencer) {
			f.proof.ActiveReleases = []string{"web-old"}
		}},
		{"rollback", func(_ *testing.T, _ *pgxpool.Pool, _ *generated.Project, f *testSourceMapRetirementFencer) {
			f.proof.RollbackReleases = []string{"web-old"}
		}},
		{"recent event", func(t *testing.T, pool *pgxpool.Pool, project *generated.Project, _ *testSourceMapRetirementFencer) {
			if _, err := pool.Exec(context.Background(), `INSERT INTO events (id,project_id,org_id,release,received_at) VALUES ($1,$2,$3,$4,now())`, id.New(), project.ID, project.OrgID, "web-old"); err != nil {
				t.Fatal(err)
			}
		}},
		{"recent deploy marker", func(t *testing.T, pool *pgxpool.Pool, project *generated.Project, _ *testSourceMapRetirementFencer) {
			if _, err := pool.Exec(context.Background(), `INSERT INTO releases (id,project_id,org_id,version,deploy_marked_at) VALUES ($1,$2,$3,$4,now())`, id.New(), project.ID, project.OrgID, "web-old"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestDB(t)
			project := seedProjectForIngest(t, pool)
			bundle := seedOldRetirementBundle(t, pool, project, "web-old")
			f := retirementProof(project)
			tc.set(t, pool, project, f)
			s := serverForDB(pool)
			s.sourceMapRetirementFencer = f
			if rec := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, bundle.Digest); rec.Code != http.StatusConflict {
				t.Fatalf("protected release = %d %s", rec.Code, rec.Body.String())
			}
			if f.acquired != 1 || f.released != 1 {
				t.Fatalf("deployment fence lifecycle = %d/%d", f.acquired, f.released)
			}
			assertRetirementUnchanged(t, pool, project, bundle)
		})
	}
}

func TestSourceMapRetirementCommitsReceiptAuditAndExactMapSet(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	bundle := seedOldRetirementBundle(t, pool, project, "web-old")
	s := serverForDB(pool)
	f := retirementProof(project)
	s.sourceMapRetirementFencer = f
	if rec := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, "sha256:"+strings.Repeat("0", 64)); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("wrong digest = %d %s", rec.Code, rec.Body.String())
	}
	assertRetirementUnchanged(t, pool, project, bundle)
	rec := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, bundle.Digest)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), bundle.Digest) {
		t.Fatalf("retirement = %d %s", rec.Code, rec.Body.String())
	}
	if f.acquired != 1 || f.released != 1 {
		t.Fatalf("deployment fence lifecycle = %d/%d", f.acquired, f.released)
	}
	if countMapBundles(t, pool, project, bundle.Release) != 0 {
		t.Fatal("bundle survived successful retirement")
	}
	if count, _ := sourceMapUsage(t, pool, project); count != 0 {
		t.Fatalf("source maps survived successful retirement: %d", count)
	}
	var retired, audited int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM source_map_bundle_retirements
		WHERE bundle_id=$1 AND project_id=$2 AND org_id=$3 AND release=$4 AND digest=$5
		AND artifact_count=$6 AND total_bytes=$7 AND actor_user_id='human-admin'
		AND protection_ref='stage-retirement-fence-1' AND protection_generation='generation-1'
		AND event_policy_ref='browser-lifetime-policy-1'
		AND last_valid_event_at < now()-interval '30 days'`,
		bundle.ID, project.ID, project.OrgID, bundle.Release, bundle.Digest, bundle.ArtifactCount, bundle.TotalBytes).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE org_id=$1 AND target=$2 AND action='sourcemap.bundle.retire' AND actor_user_id='human-admin'`, project.OrgID, bundle.ID).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if retired != 1 || audited != 1 {
		t.Fatalf("committed retirement=%d audit=%d", retired, audited)
	}
	if got := getRetirementRequest(s, project.ID, project.OrgID, bundle.ID); got.Code != http.StatusNotFound {
		t.Fatalf("bundle ID should not be a retirement ID: %d", got.Code)
	}
	if replay := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, bundle.Digest); replay.Code != http.StatusOK || replay.Body.String() != rec.Body.String() {
		t.Fatalf("exact retirement replay = %d %s", replay.Code, replay.Body.String())
	}
	if f.acquired != 1 || f.released != 1 {
		t.Fatalf("safe replay reacquired deployment fence: %d/%d", f.acquired, f.released)
	}
	if replay := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, "sha256:"+strings.Repeat("0", 64)); replay.Code != http.StatusPreconditionFailed {
		t.Fatalf("changed-digest retirement replay = %d %s", replay.Code, replay.Body.String())
	}
	var retirementID string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM source_map_bundle_retirements WHERE bundle_id=$1`, bundle.ID).Scan(&retirementID); err != nil {
		t.Fatal(err)
	}
	if got := getRetirementRequest(s, project.ID, project.OrgID, retirementID); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), bundle.Digest) {
		t.Fatalf("readback = %d %s", got.Code, got.Body.String())
	}
	if got := getRetirementRequest(s, project.ID, "foreign-org", retirementID); got.Code != http.StatusNotFound {
		t.Fatalf("foreign readback = %d %s", got.Code, got.Body.String())
	}
}

func TestSourceMapRetirementAuditFailureRollsBackDeletion(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	bundle := seedOldRetirementBundle(t, pool, project, "web-old")
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_retirement_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.action='sourcemap.bundle.retire' THEN RAISE EXCEPTION 'audit unavailable'; END IF;
		RETURN NEW; END; $$;
		CREATE TRIGGER reject_retirement_audit BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION reject_retirement_audit()`); err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	s.sourceMapRetirementFencer = retirementProof(project)
	if rec := retireBundleRequest(s, project.ID, project.OrgID, "human-admin", bundle.ID, bundle.Digest); rec.Code != http.StatusInternalServerError {
		t.Fatalf("audit failure = %d %s", rec.Code, rec.Body.String())
	}
	assertRetirementUnchanged(t, pool, project, bundle)
}
