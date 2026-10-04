package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/automationoutbox"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/metrics"
)

// alertClaim runs inside the same transaction as every automation enqueue.
// A zero row count means a competing replica already claimed the alert.
type alertClaim func(*generated.Queries) (int64, error)

// dispatchWithAutomationClaim commits all automation notices with the alert's
// cooldown/spike/monitor claim. Ordinary channels retain their existing
// best-effort behavior after commit and never enter the retry queue.
func (s *Server) dispatchWithAutomationClaim(ctx context.Context, org, project string, channels []alerts.Channel, n alerts.Notification, claim alertClaim) (bool, error) {
	if n.ProjectID == "" {
		n.ProjectID = project
	}
	var automation, ordinary []alerts.Channel
	for _, ch := range channels {
		if ch.Type == "automation_webhook" {
			automation = append(automation, ch)
		} else {
			ordinary = append(ordinary, ch)
		}
	}
	if s.pool == nil || s.automationOutbox == nil {
		if len(automation) > 0 || n.EvaluationID != "" {
			return false, errors.New("automation outbox unavailable")
		}
		if claim != nil {
			rows, err := claim(s.q)
			if err != nil || rows == 0 {
				return false, err
			}
		}
		s.dispatcher.Dispatch(ctx, ordinary, n)
		return true, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin automation alert claim: %w", err)
	}
	defer tx.Rollback(ctx)
	if claim != nil {
		rows, err := claim(s.q.WithTx(tx))
		if err != nil || rows == 0 {
			return false, err
		}
	}
	observedAt := n.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	// The wire contract and table both cap this counter. A long-lived issue
	// must still produce an alert when its lifetime count exceeds the cap.
	eventCount := max(int64(0), min(n.EventCount, int64(1_000_000_000)))
	for _, ch := range automation {
		noticeID := ""
		if n.EvaluationID != "" {
			noticeID = n.EvaluationID + ":" + ch.ID
		}
		_, err := s.automationOutbox.EnqueueTx(ctx, tx, automationoutbox.Notice{
			ID:    noticeID,
			OrgID: org, ProjectID: project, ChannelID: ch.ID, IssueID: n.IssueID,
			Event: alerts.AutomationWebhookEvent(n.Reason), EventCount: eventCount,
			ObservedAt: observedAt,
		})
		if err != nil {
			metrics.Inc("flare_automation_outbox_writes", map[string]string{"outcome": "failed"})
			return false, err
		}
	}
	if err := completeIssueEvaluationTx(ctx, tx, n.EvaluationID, org, project, n.EvaluationLeaseToken); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		metrics.Inc("flare_automation_outbox_writes", map[string]string{"outcome": "failed"})
		return false, fmt.Errorf("commit automation alert claim: %w", err)
	}
	if len(automation) > 0 {
		metrics.Add("flare_automation_outbox_writes", map[string]string{"outcome": "accepted"}, uint64(len(automation)))
	}
	s.dispatcher.Dispatch(ctx, ordinary, n)
	return true, nil
}

// RunAutomationOutbox is safe to run on every Flare replica. PostgreSQL claims
// fence each delivery attempt, and expired leases are recovered after restart.
func (s *Server) RunAutomationOutbox(ctx context.Context) {
	if s.automationOutbox == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		s.automationOutboxTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) automationOutboxTick(ctx context.Context) {
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	batch, err := s.automationOutbox.ClaimDue(claimCtx, automationoutbox.BatchSize, automationoutbox.LeaseTime)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("claim automation notices", "error", err)
		}
		return
	}
	var wg sync.WaitGroup
	for _, n := range batch {
		wg.Add(1)
		go func(n automationoutbox.Notice) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("automation delivery panicked", "notice_id", n.ID, "panic", r)
				}
			}()
			s.deliverAutomationNotice(ctx, n)
		}(n)
	}
	wg.Wait()
}

func (s *Server) deliverAutomationNotice(parent context.Context, n automationoutbox.Notice) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	config, err := s.automationOutbox.CurrentChannel(ctx, n)
	cancelled := errors.Is(err, pgx.ErrNoRows)
	delivered := false
	if err == nil {
		ch := alerts.Channel{ID: n.ChannelID, OrgID: n.OrgID, Type: "automation_webhook", Config: s.decryptChannelConfig("automation_webhook", json.RawMessage(config))}
		err = s.dispatcher.DispatchOne(ctx, ch, alerts.Notification{
			ProjectID: n.ProjectID, IssueID: n.IssueID, EventCount: n.EventCount,
			AutomationEvent: n.Event, ObservedAt: n.ObservedAt,
		})
		delivered = err == nil
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer finishCancel()
	if finishErr := s.automationOutbox.Finish(finishCtx, n, delivered, cancelled); finishErr != nil {
		// Expired leases are retried; never mark a stale attempt as accepted.
		slog.Warn("finish automation notice", "notice_id", n.ID, "error", finishErr)
		metrics.Inc("flare_automation_outbox_finishes", map[string]string{"outcome": "failed"})
		return
	}
	outcome := "retry"
	if delivered {
		outcome = "delivered"
	} else if cancelled {
		outcome = "cancelled"
	} else if n.Attempts >= automationoutbox.MaxAttempts {
		outcome = "dead"
	}
	metrics.Inc("flare_automation_outbox_finishes", map[string]string{"outcome": outcome})
	if outcome == "dead" {
		slog.Error("automation notice exhausted retries", "notice_id", n.ID, "org_id", n.OrgID, "project_id", n.ProjectID)
	} else if err != nil && outcome == "retry" {
		slog.Warn("automation notice retry scheduled", "notice_id", n.ID, "attempt", n.Attempts)
	}
}
