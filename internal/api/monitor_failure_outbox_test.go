package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/secretbox"
)

func monitorCheckinParams(org, project, slug, state string, at time.Time) generated.UpsertMonitorCheckinParams {
	status := "ok"
	if state == "failed" {
		status = "error"
	}
	return generated.UpsertMonitorCheckinParams{
		ID: id.New(), OrgID: org, ProjectID: project, Slug: slug,
		LastPingAt: pgtype.Timestamptz{Time: at, Valid: true},
		LastStatus: status, State: state,
	}
}

func TestMonitorFailureOutboxRollbackAndRecovery(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, _, projects := seedOrgProjectChannel(t, pool, 1)
	s := serverForDB(pool)
	var key, dsnID string
	if err := pool.QueryRow(ctx, `SELECT public_key,dsn_id FROM projects WHERE id=$1 AND org_id=$2`, projects[0], org).Scan(&key, &dsnID); err != nil {
		t.Fatal(err)
	}
	checkin := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/"+dsnID+"/checkins/nightly?sentry_key="+key+"&status=error", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("projectID", dsnID)
		rctx.URLParams.Add("slug", "nightly")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		s.handleCheckin(rec, req)
		return rec.Code
	}
	_, err := pool.Exec(ctx, `CREATE FUNCTION fail_monitor_queue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `CREATE TRIGGER fail_monitor_queue BEFORE INSERT ON monitor_failure_outbox FOR EACH ROW EXECUTE FUNCTION fail_monitor_queue()`)
	if err != nil {
		t.Fatal(err)
	}
	if status := checkin(); status != http.StatusServiceUnavailable {
		t.Fatalf("queue failure accepted at HTTP boundary: %d", status)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE org_id=$1 AND project_id=$2`, org, projects[0]).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed state committed despite queue error: count=%d err=%v", count, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_monitor_queue ON monitor_failure_outbox`); err != nil {
		t.Fatal(err)
	}
	if status := checkin(); status != http.StatusOK {
		t.Fatalf("retry failed check-in HTTP status: %d", status)
	}
	var monitorState, noticeState string
	if err := pool.QueryRow(ctx, `SELECT m.state,o.state FROM monitors m JOIN monitor_failure_outbox o ON o.monitor_id=m.id WHERE m.org_id=$1 AND m.project_id=$2`, org, projects[0]).Scan(&monitorState, &noticeState); err != nil {
		t.Fatal(err)
	}
	if monitorState != "failed" || noticeState != "pending" {
		t.Fatalf("expected failed monitor and pending notice, got %s / %s", monitorState, noticeState)
	}
}

func TestMonitorFailureOutboxGenerationsAndRecovery(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, _, projects := seedOrgProjectChannel(t, pool, 1)
	s := serverForDB(pool)
	s.dispatcher = alerts.NewDispatcher(nil)
	channels, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: projects[0]})
	if err != nil {
		t.Fatal(err)
	}
	firstAt := time.Now().UTC().Add(-time.Minute)
	if err := s.recordFailedMonitorCheckin(ctx, monitorCheckinParams(org, projects[0], "flapping", "failed", firstAt), channels); err != nil {
		t.Fatal(err)
	}
	// First notice is leased, then the job recovers and fails again before
	// delivery. Only the latest episode remains pending; the stale finish is
	// fenced and cannot claim it was delivered.
	first, err := s.claimMonitorFailureNotices(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %v count=%d", err, len(first))
	}
	if _, err := s.q.UpsertMonitorCheckin(ctx, monitorCheckinParams(org, projects[0], "flapping", "ok", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	secondAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.recordFailedMonitorCheckin(ctx, monitorCheckinParams(org, projects[0], "flapping", "failed", secondAt), channels); err != nil {
		t.Fatal(err)
	}
	if err := s.finishMonitorFailureNotice(ctx, first[0], "delivered"); err != nil {
		t.Fatal(err)
	}
	var state string
	var generation int64
	var count int
	var observed time.Time
	if err := pool.QueryRow(ctx, `SELECT state,failed_generation,observed_at FROM monitor_failure_outbox WHERE org_id=$1 AND project_id=$2`, org, projects[0]).Scan(&state, &generation, &observed); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || generation != 2 || !observed.Equal(secondAt) {
		t.Fatalf("new failure notice lost or stale finish accepted: state=%s generation=%d at=%v", state, generation, observed)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monitor_failure_outbox WHERE org_id=$1 AND project_id=$2`, org, projects[0]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unbounded rows: %d %v", count, err)
	}
	second, err := s.claimMonitorFailureNotices(ctx)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim: %v count=%d", err, len(second))
	}
	// A success after an accepted failure must not erase its pending notice.
	if _, err := s.q.UpsertMonitorCheckin(ctx, monitorCheckinParams(org, projects[0], "flapping", "ok", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	s.deliverMonitorFailureNotice(ctx, second[0])
	if err := pool.QueryRow(ctx, `SELECT state FROM monitor_failure_outbox WHERE org_id=$1 AND project_id=$2`, org, projects[0]).Scan(&state); err != nil || state != "delivered" {
		t.Fatalf("recovered failure notice was not delivered: %s %v", state, err)
	}
}

func TestMonitorFailureOutboxRoutingAndRetry(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, chID, projects := seedOrgProjectChannel(t, pool, 2)
	s := serverForDB(pool)
	s.dispatcher = alerts.NewDispatcher(nil)
	if err := s.q.AddChannelProject(ctx, generated.AddChannelProjectParams{ChannelID: chID, ProjectID: projects[0]}); err != nil {
		t.Fatal(err)
	}
	for i, pid := range projects {
		channels, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: pid})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && len(channels) != 0 {
			t.Fatal("project B inherited project A route")
		}
		if i == 0 {
			if len(channels) != 1 {
				t.Fatal("project A route missing")
			}
			if err := s.recordFailedMonitorCheckin(ctx, monitorCheckinParams(org, pid, "pipeline", "failed", time.Now().UTC()), channels); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The route changes before delivery; the old project must not receive it.
	batch, err := s.claimMonitorFailureNotices(ctx)
	if err != nil || len(batch) != 1 {
		t.Fatalf("claim: %v %d", err, len(batch))
	}
	if _, err := pool.Exec(ctx, `DELETE FROM channel_projects WHERE channel_id=$1`, chID); err != nil {
		t.Fatal(err)
	}
	if err := s.q.AddChannelProject(ctx, generated.AddChannelProjectParams{ChannelID: chID, ProjectID: projects[1]}); err != nil {
		t.Fatal(err)
	}
	s.deliverMonitorFailureNotice(ctx, batch[0])
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM monitor_failure_outbox WHERE channel_id=$1`, chID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("revoked route delivered: %s %v", state, err)
	}
	// A new error ping after restoring the route revives the cancelled row;
	// a broken destination remains pending and can be repaired.
	if _, err := pool.Exec(ctx, `DELETE FROM channel_projects WHERE channel_id=$1`, chID); err != nil {
		t.Fatal(err)
	}
	if err := s.q.AddChannelProject(ctx, generated.AddChannelProjectParams{ChannelID: chID, ProjectID: projects[0]}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET type='email',config=$2::jsonb WHERE id=$1`, chID, json.RawMessage(`{"to":"ops@example.test"}`)); err != nil {
		t.Fatal(err)
	}
	channels, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: projects[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordFailedMonitorCheckin(ctx, monitorCheckinParams(org, projects[0], "pipeline", "failed", time.Now().UTC()), channels); err != nil {
		t.Fatal(err)
	}
	batch, err = s.claimMonitorFailureNotices(ctx)
	if err != nil || len(batch) != 1 {
		t.Fatalf("retry claim: %v %d", err, len(batch))
	}
	s.deliverMonitorFailureNotice(ctx, batch[0])
	var attempts int
	var next time.Time
	if err := pool.QueryRow(ctx, `SELECT state,attempts,next_attempt_at FROM monitor_failure_outbox WHERE channel_id=$1`, chID).Scan(&state, &attempts, &next); err != nil || state != "pending" || attempts != 1 || !next.After(time.Now()) {
		t.Fatalf("failed delivery lost: %s %d %v %v", state, attempts, next, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET type='log',config='{}'::jsonb WHERE id=$1`, chID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE monitor_failure_outbox SET next_attempt_at=now()-interval '1 second' WHERE channel_id=$1`, chID); err != nil {
		t.Fatal(err)
	}
	batch, err = s.claimMonitorFailureNotices(ctx)
	if err != nil || len(batch) != 1 {
		t.Fatalf("reclaim: %v %d", err, len(batch))
	}
	s.deliverMonitorFailureNotice(ctx, batch[0])
	if err := pool.QueryRow(ctx, `SELECT state FROM monitor_failure_outbox WHERE channel_id=$1`, chID).Scan(&state); err != nil || state != "delivered" {
		t.Fatalf("repaired destination not delivered: %s %v", state, err)
	}
	// Disable after a later generation and verify current enablement is used.
	if _, err := s.q.UpsertMonitorCheckin(ctx, monitorCheckinParams(org, projects[0], "pipeline", "ok", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if err := s.recordFailedMonitorCheckin(ctx, monitorCheckinParams(org, projects[0], "pipeline", "failed", time.Now().UTC()), channels); err != nil {
		t.Fatal(err)
	}
	batch, err = s.claimMonitorFailureNotices(ctx)
	if err != nil || len(batch) != 1 {
		t.Fatalf("disable claim: %v %d", err, len(batch))
	}
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET enabled=false WHERE id=$1`, chID); err != nil {
		t.Fatal(err)
	}
	s.deliverMonitorFailureNotice(ctx, batch[0])
	if err := pool.QueryRow(ctx, `SELECT state FROM monitor_failure_outbox WHERE channel_id=$1`, chID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("disabled channel delivered: %s %v", state, err)
	}
}

func TestMonitorFailureOutboxConcurrentCheckinsAndExpiredLease(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, _, projects := seedOrgProjectChannel(t, pool, 1)
	s := serverForDB(pool)
	channels, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: projects[0]})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errors <- s.recordFailedMonitorCheckin(ctx,
				monitorCheckinParams(org, projects[0], "concurrent", "failed", time.Now().UTC()), channels)
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT count(*),max(failed_generation) FROM monitor_failure_outbox WHERE org_id=$1 AND project_id=$2`, org, projects[0]).Scan(&count, &generation); err != nil || count != 1 || generation != 1 {
		t.Fatalf("concurrent check-ins duplicated failure: rows=%d generation=%d err=%v", count, generation, err)
	}
	first, err := s.claimMonitorFailureNotices(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %v count=%d", err, len(first))
	}
	second, err := s.claimMonitorFailureNotices(ctx)
	if err != nil || len(second) != 0 {
		t.Fatalf("unexpired lease was reclaimed: %v count=%d", err, len(second))
	}
	if _, err := pool.Exec(ctx, `UPDATE monitor_failure_outbox SET leased_until=now()-interval '1 second' WHERE org_id=$1 AND project_id=$2`, org, projects[0]); err != nil {
		t.Fatal(err)
	}
	second, err = s.claimMonitorFailureNotices(ctx)
	if err != nil || len(second) != 1 || first[0].leaseToken == second[0].leaseToken {
		t.Fatalf("expired lease not fenced and reclaimed: %v count=%d", err, len(second))
	}
	if err := s.finishMonitorFailureNotice(ctx, first[0], "delivered"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM monitor_failure_outbox WHERE org_id=$1 AND project_id=$2`, org, projects[0]).Scan(&state); err != nil || state != "leased" {
		t.Fatalf("stale lease claimed delivery: %s %v", state, err)
	}
	health, err := s.monitorFailureOutboxHealth(ctx)
	if err != nil || health.ExpiredLeases != 0 || health.Pending != 0 {
		t.Fatalf("unexpected leased health: %#v %v", health, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE monitor_failure_outbox SET leased_until=now()-interval '10 minutes' WHERE org_id=$1 AND project_id=$2`, org, projects[0]); err != nil {
		t.Fatal(err)
	}
	health, err = s.monitorFailureOutboxHealth(ctx)
	if err != nil || health.ExpiredLeases != 1 || health.OldestDueSeconds < 590 {
		t.Fatalf("stopped worker not visible in bounded health: %#v %v", health, err)
	}
}

func TestMonitorFailureOutboxAutomationWebhookRetryKeepsFixedErrorCode(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, chID, projects := seedOrgProjectChannel(t, pool, 1)
	s := serverForDB(pool)
	s.dispatcher = alerts.NewDispatcher(nil)
	s.secrets = secretbox.New("")
	const credential = "credential-marker-do-not-store"
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET type='automation_webhook',config=$2::jsonb WHERE id=$1`,
		chID, json.RawMessage(`{"url":"http://127.0.0.1:1/path/`+credential+`","signing_secret":"`+credential+`"}`)); err != nil {
		t.Fatal(err)
	}
	channels, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: projects[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordFailedMonitorCheckin(ctx, monitorCheckinParams(org, projects[0], "hook", "failed", time.Now().UTC()), channels); err != nil {
		t.Fatal(err)
	}
	batch, err := s.claimMonitorFailureNotices(ctx)
	if err != nil || len(batch) != 1 {
		t.Fatalf("claim: %v count=%d", err, len(batch))
	}
	s.deliverMonitorFailureNotice(ctx, batch[0])
	var state, code string
	if err := pool.QueryRow(ctx, `SELECT state,last_error_code FROM monitor_failure_outbox WHERE channel_id=$1`, chID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || code != "delivery_failed" {
		t.Fatalf("automation webhook attempt was not retried with fixed code: %s/%s", state, code)
	}
	var leaked bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM monitor_failure_outbox WHERE channel_id=$1 AND last_error_code LIKE '%' || $2 || '%')`, chID, credential).Scan(&leaked); err != nil || leaked {
		t.Fatalf("credential in outbox error code: %v %v", leaked, err)
	}
}
