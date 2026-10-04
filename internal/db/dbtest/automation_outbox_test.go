package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/automationoutbox"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func automationOutboxChannel(t *testing.T, ctx context.Context, q *generated.Queries, org string) string {
	t.Helper()
	channelID := id.New()
	_, err := q.CreateNotificationChannel(ctx, generated.CreateNotificationChannelParams{
		ID: channelID, OrgID: org, Type: "automation_webhook", Config: json.RawMessage(`{"url":"https://worker.example.test/alert","signing_secret":"0123456789abcdef"}`), Enabled: true,
	})
	if err != nil {
		t.Fatalf("create automation channel: %v", err)
	}
	return channelID
}

func outboxNotice(org, project, channel string) automationoutbox.Notice {
	return automationoutbox.Notice{
		OrgID: org, ProjectID: project, ChannelID: channel,
		Event: "new_issue", EventCount: 2, ObservedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func TestAutomationOutboxClaimAndInsertAreAtomicAndTenantScoped(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	otherOrg, otherProject := seedProject(t, ctx, q)
	channel := automationOutboxChannel(t, ctx, q, org)
	store := automationoutbox.New(pool)
	ruleID := id.New()
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules(id,org_id,project_id,name,type) VALUES($1,$2,$3,'test','new_issue')`, ruleID, org, project); err != nil {
		t.Fatalf("seed alert rule: %v", err)
	}
	claimAndEnqueue := func(commit bool) string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		if err := tx.QueryRow(ctx, `UPDATE alert_rules SET last_fired_at=now() WHERE id=$1 AND org_id=$2 RETURNING 1`, ruleID, org).Scan(&n); err != nil || n != 1 {
			t.Fatalf("claim rule: %v", err)
		}
		queuedID, err := store.EnqueueTx(ctx, tx, outboxNotice(org, project, channel))
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		return queuedID
	}
	rolledID := claimAndEnqueue(false)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM automation_alert_outbox WHERE id=$1`, rolledID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back queue count = %d, %v", count, err)
	}
	var lastFired *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_fired_at FROM alert_rules WHERE id=$1`, ruleID).Scan(&lastFired); err != nil || lastFired != nil {
		t.Fatalf("rolled back claim = %v, %v", lastFired, err)
	}
	committedID := claimAndEnqueue(true)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM automation_alert_outbox WHERE id=$1`, committedID) })
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM automation_alert_outbox WHERE id=$1 AND org_id=$2 AND project_id=$3`, committedID, org, project).Scan(&count); err != nil || count != 1 {
		t.Fatalf("committed queue count = %d, %v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT last_fired_at FROM alert_rules WHERE id=$1`, ruleID).Scan(&lastFired); err != nil || lastFired == nil {
		t.Fatalf("committed claim = %v, %v", lastFired, err)
	}

	// A project from another org cannot be paired with this channel, even if
	// the caller knows both IDs. The application lookup and composite FKs each
	// enforce the boundary independently.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.EnqueueTx(ctx, tx, outboxNotice(otherOrg, otherProject, channel))
	if !errors.Is(err, automationoutbox.ErrRouteUnavailable) {
		t.Fatalf("cross-tenant enqueue = %v", err)
	}
	_ = tx.Rollback(ctx)
	_, err = pool.Exec(ctx, `INSERT INTO automation_alert_outbox(id,org_id,project_id,channel_id,event,observed_at) VALUES($1,$2,$3,$4,'alert',now())`, id.New(), otherOrg, otherProject, channel)
	if err == nil {
		t.Fatal("composite channel foreign key admitted another org's channel")
	}
	_, err = pool.Exec(ctx, `INSERT INTO automation_alert_outbox(id,org_id,project_id,channel_id,event,observed_at) VALUES($1,$2,$3,$4,'alert',now())`, id.New(), otherOrg, project, channel)
	if err == nil {
		t.Fatal("composite project foreign key admitted another org's project")
	}
}

func TestAutomationOutboxLeaseRecoveryAndRouting(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	channel := automationOutboxChannel(t, ctx, q, org)
	store := automationoutbox.New(pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	noticeID, err := store.EnqueueTx(ctx, tx, outboxNotice(org, project, channel))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM automation_alert_outbox WHERE id=$1`, noticeID) })
	var wg sync.WaitGroup
	results := make(chan []automationoutbox.Notice, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := automationoutbox.New(pool).ClaimDue(ctx, 1, time.Second)
			if err != nil {
				t.Errorf("concurrent claim: %v", err)
			}
			results <- rows
		}()
	}
	wg.Wait()
	close(results)
	var first automationoutbox.Notice
	claimed := 0
	for rows := range results {
		claimed += len(rows)
		if len(rows) == 1 {
			first = rows[0]
		}
	}
	if claimed != 1 || first.ID != noticeID || first.Attempts != 1 {
		t.Fatalf("concurrent claim = %d, %#v", claimed, first)
	}
	if _, err := store.CurrentChannel(ctx, first); err != nil {
		t.Fatalf("current route missing: %v", err)
	}
	wrong := first
	wrong.LeaseToken = "wrong-token"
	if err := store.Finish(ctx, wrong, true, false); !errors.Is(err, automationoutbox.ErrStaleLease) {
		t.Fatalf("wrong token finished delivery: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE automation_alert_outbox SET leased_until=now()-interval '1 second' WHERE id=$1`, noticeID); err != nil {
		t.Fatal(err)
	}
	secondBatch, err := automationoutbox.New(pool).ClaimDue(ctx, 1, time.Second)
	if err != nil || len(secondBatch) != 1 {
		t.Fatalf("restart claim = %d, %v", len(secondBatch), err)
	}
	second := secondBatch[0]
	if second.ID != first.ID || !second.ObservedAt.Equal(first.ObservedAt) || second.Attempts != 2 || second.LeaseToken == first.LeaseToken {
		t.Fatalf("retry changed stable identity or failed to fence: %#v -> %#v", first, second)
	}
	if err := store.Finish(ctx, first, true, false); !errors.Is(err, automationoutbox.ErrStaleLease) {
		t.Fatalf("stale worker finished after restart: %v", err)
	}
	if err := store.Finish(ctx, second, false, false); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE automation_alert_outbox SET next_attempt_at=now()-interval '1 second' WHERE id=$1`, noticeID); err != nil {
		t.Fatal(err)
	}
	thirdBatch, err := store.ClaimDue(ctx, 1, time.Second)
	if err != nil || len(thirdBatch) != 1 || thirdBatch[0].ID != noticeID || thirdBatch[0].Attempts != 3 {
		t.Fatalf("scheduled retry = %#v, %v", thirdBatch, err)
	}
	// Revoking routing between enqueue and send cancels this exact notice.
	otherProject, err := q.CreateProject(ctx, generated.CreateProjectParams{ID: id.New(), OrgID: org, Name: "other", Slug: id.New(), Platform: "go", PublicKey: id.New(), DsnID: id.New()[:12]})
	if err != nil {
		t.Fatal(err)
	}
	// Routing to a different project means the original project's notice is
	// no longer authorized to leave Flare.
	if _, err := pool.Exec(ctx, `INSERT INTO channel_projects(channel_id,project_id) VALUES($1,$2)`, channel, otherProject.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CurrentChannel(ctx, thirdBatch[0]); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("rerouted notice left Flare: %v", err)
	}
	if err := store.Finish(ctx, thirdBatch[0], false, true); err != nil {
		t.Fatalf("cancel rerouted notice: %v", err)
	}
	stats, err := store.Snapshot(ctx)
	if err != nil || stats.Pending != 0 || stats.Leased != 0 || stats.Dead != 0 {
		t.Fatalf("outbox after cancel = %#v, %v", stats, err)
	}
	// A worker that crashes on its final permitted attempt must become a
	// visible dead row after lease expiry, not an invisible permanent lease.
	if _, err := pool.Exec(ctx, `UPDATE automation_alert_outbox
SET state='leased', attempts=12, lease_token='last-worker', leased_until=now()-interval '1 second'
WHERE id=$1`, noticeID); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ClaimDue(ctx, 1, time.Second); err != nil || len(rows) != 0 {
		t.Fatalf("exhausted notice retried: %#v, %v", rows, err)
	}
	stats, err = store.Snapshot(ctx)
	if err != nil || stats.Dead != 1 {
		t.Fatalf("exhausted lease not observable: %#v, %v", stats, err)
	}
}

func TestAutomationOutboxKeepsLedgerWhenChannelIsDeleted(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	channel := automationOutboxChannel(t, ctx, q, org)
	store := automationoutbox.New(pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	noticeID, err := store.EnqueueTx(ctx, tx, outboxNotice(org, project, channel))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM automation_alert_outbox WHERE id=$1`, noticeID) })
	if _, err := pool.Exec(ctx, `DELETE FROM notification_channels WHERE id=$1 AND org_id=$2`, channel, org); err != nil {
		t.Fatal(err)
	}
	var retainedChannel *string
	if err := pool.QueryRow(ctx, `SELECT channel_id FROM automation_alert_outbox WHERE id=$1 AND org_id=$2 AND project_id=$3`, noticeID, org, project).Scan(&retainedChannel); err != nil || retainedChannel != nil {
		t.Fatalf("deleted channel removed or retargeted ledger: %v, %v", retainedChannel, err)
	}
	rows, err := store.ClaimDue(ctx, 1, time.Second)
	if err != nil || len(rows) != 1 || rows[0].ID != noticeID {
		t.Fatalf("deleted-channel notice not claimable for cancellation: %#v, %v", rows, err)
	}
	if _, err := store.CurrentChannel(ctx, rows[0]); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted channel still routable: %v", err)
	}
	if err := store.Finish(ctx, rows[0], false, true); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM automation_alert_outbox WHERE id=$1`, noticeID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("deleted-channel notice = %q, %v", state, err)
	}
}
