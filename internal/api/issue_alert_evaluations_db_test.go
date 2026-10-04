package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/automationoutbox"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ratelimit"
)

func TestAcceptedIssueEventHasDurableEvaluationDespiteSaturatedBackgroundPool(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, channel, projects := seedOrgProjectChannel(t, pool, 1)
	projectID := projects[0]
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET type='automation_webhook',
		config='{"url":"https://worker.example.test/alert","signing_secret":"0123456789abcdef"}'
		WHERE id=$1`, channel); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules(id,org_id,project_id,name,type)
		VALUES($1,$2,$3,'new issue','new_issue')`, id.New(), org, projectID); err != nil {
		t.Fatal(err)
	}
	project, err := generated.New(pool).GetProjectByID(ctx, generated.GetProjectByIDParams{ID: projectID, OrgScope: org})
	if err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	s.automationOutbox = automationoutbox.New(pool)
	s.dispatcher = alerts.NewDispatcher(nil)
	s.issueAlertLimiter = ratelimit.New(100, time.Minute)
	// The old in-memory alert evaluation path dropped work when this pool was
	// full. Holding every slot now has no effect on the committed handoff.
	bg := s.bgPool("alert-eval")
	for i := 0; i < cap(bg); i++ {
		bg <- struct{}{}
	}
	t.Cleanup(func() {
		for len(bg) > 0 {
			<-bg
		}
	})
	if _, err := s.ingestOne(ctx, project, []byte(`{"event_id":"first","level":"error","message":"checkout failed"}`)); err != nil {
		t.Fatalf("ingest with saturated background pool: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s.WaitBackground(waitCtx) // auto-triage's separate best-effort job
	var jobID string
	var observedAt time.Time
	var count int64
	if err := pool.QueryRow(ctx, `SELECT id,observed_at,event_count FROM issue_alert_evaluations
		WHERE org_id=$1 AND project_id=$2`, org, projectID).Scan(&jobID, &observedAt, &count); err != nil {
		t.Fatalf("accepted event has no durable evaluation: %v", err)
	}
	if count != 1 {
		t.Fatalf("evaluation event_count = %d, want first event snapshot", count)
	}
	health, err := s.IssueEvaluationHealth(ctx)
	if err != nil || health.Pending != 1 || health.ExpiredLeases != 0 || health.Dead != 0 || health.CountsCapped {
		t.Fatalf("pending queue health = %#v, %v", health, err)
	}
	first, err := s.claimIssueEvaluations(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("first evaluation claim = %d, %v", len(first), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE issue_alert_evaluations
		SET leased_until=now()-interval '1 second' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.claimIssueEvaluations(ctx)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].LeaseToken == first[0].LeaseToken {
		t.Fatalf("expired evaluation lease was not reclaimed: %#v, %v", reclaimed, err)
	}
	s.runIssueEvaluation(ctx, first[0])
	var staleNotices int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM automation_alert_outbox WHERE org_id=$1`, org).Scan(&staleNotices); err != nil || staleNotices != 0 {
		t.Fatalf("stale evaluation committed %d notices: %v", staleNotices, err)
	}
	s.runIssueEvaluation(ctx, reclaimed[0])
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM issue_alert_evaluations WHERE id=$1`, jobID).Scan(&state); err != nil || state != "done" {
		t.Fatalf("evaluation state = %q, %v", state, err)
	}
	var noticeID, event string
	var noticeCount int64
	var noticeObserved time.Time
	if err := pool.QueryRow(ctx, `SELECT id,event,event_count,observed_at FROM automation_alert_outbox
		WHERE org_id=$1 AND project_id=$2`, org, projectID).Scan(&noticeID, &event, &noticeCount, &noticeObserved); err != nil {
		t.Fatalf("evaluation did not durably enqueue automation: %v", err)
	}
	if noticeID != jobID+":"+channel || event != "new_issue" || noticeCount != 1 || !noticeObserved.Equal(observedAt) {
		t.Fatalf("notice identity drift: id=%q event=%q count=%d observed=%v", noticeID, event, noticeCount, noticeObserved)
	}
	health, err = s.IssueEvaluationHealth(ctx)
	if err != nil || health.Pending != 0 || health.ExpiredLeases != 0 || health.Dead != 0 {
		t.Fatalf("drained queue health = %#v, %v", health, err)
	}
	s.issueEvaluationTick(ctx)
	var notices int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM automation_alert_outbox WHERE org_id=$1`, org).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("repeated tick created %d notices: %v", notices, err)
	}
}

func TestIssueEventAndEvaluationRollbackTogether(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, _, projects := seedOrgProjectChannel(t, pool, 1)
	projectID := projects[0]
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules(id,org_id,project_id,name,type)
		VALUES($1,$2,$3,'new issue','new_issue')`, id.New(), org, projectID); err != nil {
		t.Fatal(err)
	}
	project, err := generated.New(pool).GetProjectByID(ctx, generated.GetProjectByIDParams{ID: projectID, OrgScope: org})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate queue storage rejecting a write after the issue and event were
	// tentatively inserted. A 503 path must not leave either row committed.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_issue_evaluation_test() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test queue failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER fail_issue_evaluation_test BEFORE INSERT
		ON issue_alert_evaluations FOR EACH ROW EXECUTE FUNCTION fail_issue_evaluation_test()`); err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	_, err = s.ingestOne(ctx, project, []byte(`{"event_id":"rollback","level":"error","message":"rollback me"}`))
	if err == nil || !strings.Contains(err.Error(), "enqueue issue alert evaluation") {
		t.Fatalf("queue failure returned %v", err)
	}
	for _, table := range []string{"issues", "events", "issue_alert_evaluations"} {
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE org_id=$1 AND project_id=$2`, org, projectID).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("%s committed despite queue failure: %d, %v", table, rows, err)
		}
	}
}

func TestIssueEvaluationScopeAndOutboxReplay(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, channel, projects := seedOrgProjectChannel(t, pool, 1)
	otherOrg, _, otherProjects := seedOrgProjectChannel(t, pool, 1)
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET type='automation_webhook' WHERE id=$1`, channel); err != nil {
		t.Fatal(err)
	}
	issue, err := generated.New(pool).UpsertIssue(ctx, generated.UpsertIssueParams{
		ID: id.New(), OrgID: org, ProjectID: projects[0], Fingerprint: id.New(), Title: "test", Level: "error",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO issue_alert_evaluations
		(id,org_id,project_id,issue_id,is_new,reopened,event_count,observed_at)
		VALUES($1,$2,$3,$4,true,false,1,now())`, id.New(), otherOrg, otherProjects[0], issue.ID)
	if err == nil {
		t.Fatal("evaluation queue admitted an issue belonging to another tenant")
	}
	store := automationoutbox.New(pool)
	observed := time.Now().UTC().Truncate(time.Microsecond)
	notice := automationoutbox.Notice{
		ID: id.New(), OrgID: org, ProjectID: projects[0], ChannelID: channel,
		IssueID: issue.ID, Event: "new_issue", EventCount: 1, ObservedAt: observed,
	}
	insert := func(n automationoutbox.Notice) error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		_, err = store.EnqueueTx(ctx, tx, n)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := insert(notice); err != nil {
		t.Fatal(err)
	}
	if err := insert(notice); err != nil {
		t.Fatalf("exact replay should be idempotent: %v", err)
	}
	changed := notice
	changed.EventCount++
	if err := insert(changed); !errors.Is(err, automationoutbox.ErrNoticeIdentityConflict) {
		t.Fatalf("count mutation accepted on stable ID: %v", err)
	}
	changed = notice
	changed.ObservedAt = observed.Add(time.Second)
	if err := insert(changed); !errors.Is(err, automationoutbox.ErrNoticeIdentityConflict) {
		t.Fatalf("observation mutation accepted on stable ID: %v", err)
	}
	changed = notice
	changed.OrgID = otherOrg
	changed.ProjectID = otherProjects[0]
	if err := insert(changed); !errors.Is(err, automationoutbox.ErrNoticeIdentityConflict) {
		t.Fatalf("cross-tenant ID replay accepted: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM automation_alert_outbox WHERE id=$1`, notice.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("replay stored %d rows: %v", rows, err)
	}
}
