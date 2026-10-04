package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func monitorHTTPCheckin(s *Server, dsnID, key, slug, status string) int {
	req := httptest.NewRequest(http.MethodPost,
		"/api/"+dsnID+"/checkins/"+slug+"?sentry_key="+key+"&status="+status, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", dsnID)
	rctx.URLParams.Add("slug", slug)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.handleCheckin(rec, req)
	return rec.Code
}

func monitorDSN(t *testing.T, pool *pgxpool.Pool, org, project string) (dsnID, key string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT dsn_id,public_key FROM projects WHERE id=$1 AND org_id=$2`, project, org).
		Scan(&dsnID, &key); err != nil {
		t.Fatal(err)
	}
	return dsnID, key
}

func seedMonitorRows(t *testing.T, pool *pgxpool.Pool, org, project string, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO monitors (id,org_id,project_id,slug,last_ping_at,last_status,state)
SELECT 'seed-'||gs.i::text,$1,$2,'seed-'||gs.i::text,now(),'ok','ok'
FROM generate_series(1,$3::int) AS gs(i)`, org, project, n); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorCheckinConcurrentNewSlugsRespectProjectCap(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, _, projects := seedOrgProjectChannel(t, pool, 1)
	project := projects[0]
	seedMonitorRows(t, pool, org, project, maxMonitorsPerProject-5)
	dsnID, key := monitorDSN(t, pool, org, project)
	s := serverForDB(pool)

	const concurrent = 32
	start := make(chan struct{})
	results := make(chan struct {
		slug, status string
		code         int
	}, concurrent)
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			slug := fmt.Sprintf("race-%02d", i)
			status := "ok"
			if i%2 == 1 {
				status = "error"
			}
			results <- struct {
				slug, status string
				code         int
			}{slug, status, monitorHTTPCheckin(s, dsnID, key, slug, status)}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, refused, failedAccepted := 0, 0, 0
	for got := range results {
		switch got.code {
		case http.StatusOK:
			accepted++
			if got.status == "error" {
				failedAccepted++
			}
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Fatalf("%s check-in returned unexpected %d", got.slug, got.code)
		}
	}
	if accepted != 5 || refused != concurrent-5 {
		t.Fatalf("quota race admitted %d new slugs, refused %d; expected 5/%d", accepted, refused, concurrent-5)
	}
	var total, notices, failedRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE org_id=$1 AND project_id=$2`, org, project).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE org_id=$1 AND project_id=$2 AND state='failed'`, org, project).Scan(&failedRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitor_failure_outbox WHERE org_id=$1 AND project_id=$2 AND state='pending'`, org, project).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if total != maxMonitorsPerProject || failedRows != failedAccepted || notices != failedAccepted {
		t.Fatalf("monitor/notice mismatch after race: total=%d failed=%d notices=%d accepted failures=%d", total, failedRows, notices, failedAccepted)
	}
	// Existing slugs remain usable at capacity. A rejected fresh failure
	// creates neither a monitor nor an alert notice.
	if got := monitorHTTPCheckin(s, dsnID, key, "seed-1", "error"); got != http.StatusOK {
		t.Fatalf("existing failed check-in at capacity: %d", got)
	}
	if got := monitorHTTPCheckin(s, dsnID, key, "seed-1", "ok"); got != http.StatusOK {
		t.Fatalf("existing successful check-in at capacity: %d", got)
	}
	if got := monitorHTTPCheckin(s, dsnID, key, "overflow-failed", "error"); got != http.StatusTooManyRequests {
		t.Fatalf("new failed check-in at capacity: %d", got)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE org_id=$1 AND project_id=$2`, org, project).Scan(&total); err != nil || total != maxMonitorsPerProject {
		t.Fatalf("at-cap rows changed: %d %v", total, err)
	}
	// The authenticated dashboard create path obeys the same project cap.
	createReq := httptest.NewRequest(http.MethodPost, "/projects/"+project+"/monitors",
		strings.NewReader(`{"slug":"dashboard-overflow","interval_seconds":60}`))
	createCtx := chi.NewRouteContext()
	createCtx.URLParams.Add("id", project)
	createReq = createReq.WithContext(context.WithValue(
		context.WithValue(createReq.Context(), chi.RouteCtxKey, createCtx), ctxOrgID, org))
	createRec := httptest.NewRecorder()
	s.handleCreateMonitor(createRec, createReq)
	if createRec.Code != http.StatusTooManyRequests {
		t.Fatalf("dashboard create bypassed project cap: %d", createRec.Code)
	}
	// A fresh unconfigured monitor awaiting its first ping is retained. Only
	// after 30 quiet days may quota admission prune it and reuse the slot.
	if _, err := pool.Exec(ctx, `UPDATE monitors SET last_ping_at=NULL WHERE org_id=$1 AND project_id=$2 AND slug='seed-2'`, org, project); err != nil {
		t.Fatal(err)
	}
	if got := monitorHTTPCheckin(s, dsnID, key, "before-prune-age", "ok"); got != http.StatusTooManyRequests {
		t.Fatalf("fresh unconfigured no-ping monitor was pruned: %d", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE monitors SET created_at=now()-interval '31 days' WHERE org_id=$1 AND project_id=$2 AND slug='seed-2'`, org, project); err != nil {
		t.Fatal(err)
	}
	if got := monitorHTTPCheckin(s, dsnID, key, "after-prune-age", "ok"); got != http.StatusOK {
		t.Fatalf("stale unconfigured monitor did not free its slot: %d", got)
	}
	var stale int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE org_id=$1 AND project_id=$2 AND slug='seed-2'`, org, project).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("stale unconfigured monitor retained: %d %v", stale, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE org_id=$1 AND project_id=$2`, org, project).Scan(&total); err != nil || total != maxMonitorsPerProject {
		t.Fatalf("prune/recreate count changed: %d %v", total, err)
	}
}

func TestMonitorCheckinQuotaAndFailedAlertRollBackTogether(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, _, projects := seedOrgProjectChannel(t, pool, 1)
	project := projects[0]
	seedMonitorRows(t, pool, org, project, maxMonitorsPerProject-1)
	dsnID, key := monitorDSN(t, pool, org, project)
	s := serverForDB(pool)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_monitor_queue_cap() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER fail_monitor_queue_cap BEFORE INSERT ON monitor_failure_outbox FOR EACH ROW EXECUTE FUNCTION fail_monitor_queue_cap()`); err != nil {
		t.Fatal(err)
	}
	if got := monitorHTTPCheckin(s, dsnID, key, "last-slot-failed", "error"); got != http.StatusServiceUnavailable {
		t.Fatalf("failed alert enqueue must roll back new monitor: %d", got)
	}
	var total, notices int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE org_id=$1 AND project_id=$2`, org, project).Scan(&total); err != nil || total != maxMonitorsPerProject-1 {
		t.Fatalf("failed enqueue consumed slot: %d %v", total, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_monitor_queue_cap ON monitor_failure_outbox`); err != nil {
		t.Fatal(err)
	}
	if got := monitorHTTPCheckin(s, dsnID, key, "last-slot-failed", "error"); got != http.StatusOK {
		t.Fatalf("retry must accept final slot and alert together: %d", got)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE org_id=$1 AND project_id=$2`, org, project).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitor_failure_outbox WHERE org_id=$1 AND project_id=$2 AND state='pending'`, org, project).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if total != maxMonitorsPerProject || notices != 1 {
		t.Fatalf("final-slot failure lacks atomic notice: monitors=%d notices=%d", total, notices)
	}
}
