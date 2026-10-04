package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/automationoutbox"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// This drives the production claim/queue helper against PostgreSQL 16. If a
// route disappears after channel loading, the rule's cooldown must not be
// committed without its automation notice.
func TestAutomationOutboxRuleClaimRollsBackWhenRouteDisappears(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, chID, projects := seedOrgProjectChannel(t, pool, 2)
	project := projects[0]
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET type='automation_webhook' WHERE id=$1`, chID); err != nil {
		t.Fatal(err)
	}
	ruleID := id.New()
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules(id,org_id,project_id,name,type) VALUES($1,$2,$3,'test','silence')`, ruleID, org, project); err != nil {
		t.Fatal(err)
	}
	s := &Server{q: generated.New(pool), pool: pool, automationOutbox: automationoutbox.New(pool), dispatcher: alerts.NewDispatcher(nil)}
	ch := alerts.Channel{ID: chID, OrgID: org, Type: "automation_webhook"}
	claim := func(q *generated.Queries) (int64, error) {
		return q.TrySetAlertRuleFired(ctx, generated.TrySetAlertRuleFiredParams{
			ID: ruleID, OrgID: org, LastFiredAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		})
	}
	// Channel was loaded while it served this project; an administrator now
	// routes it to the other project before the claim/queue transaction.
	if _, err := pool.Exec(ctx, `INSERT INTO channel_projects(channel_id,project_id) VALUES($1,$2)`, chID, projects[1]); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.dispatchWithAutomationClaim(ctx, org, project, []alerts.Channel{ch}, alerts.Notification{ProjectID: project, Reason: "Silence"}, claim)
	if claimed || !errors.Is(err, automationoutbox.ErrRouteUnavailable) {
		t.Fatalf("rerouted claim result = %v, %v", claimed, err)
	}
	var lastFired *time.Time
	var count int
	if err := pool.QueryRow(ctx, `SELECT last_fired_at FROM alert_rules WHERE id=$1`, ruleID).Scan(&lastFired); err != nil || lastFired != nil {
		t.Fatalf("cooldown spent without notice: %v, %v", lastFired, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM automation_alert_outbox WHERE org_id=$1 AND project_id=$2`, org, project).Scan(&count); err != nil || count != 0 {
		t.Fatalf("notice written for removed route: %d, %v", count, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM channel_projects WHERE channel_id=$1`, chID); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.dispatchWithAutomationClaim(ctx, org, project, []alerts.Channel{ch}, alerts.Notification{ProjectID: project, Reason: "Silence"}, claim)
	if err != nil || !claimed {
		t.Fatalf("restored route claim = %v, %v", claimed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM automation_alert_outbox WHERE org_id=$1 AND project_id=$2 AND event='silence'`, org, project).Scan(&count); err != nil || count != 1 {
		t.Fatalf("committed notice = %d, %v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT last_fired_at FROM alert_rules WHERE id=$1`, ruleID).Scan(&lastFired); err != nil || lastFired == nil {
		t.Fatalf("committed claim = %v, %v", lastFired, err)
	}
}
