package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// recordFailedMonitorCheckin commits the failed state and every currently
// routed notice together. A failed queue write rolls the check-in back, so a
// caller can retry an ambiguous response without silently losing the alert.
func (s *Server) recordFailedMonitorCheckin(ctx context.Context, params generated.UpsertMonitorCheckinParams, channels []*generated.NotificationChannel) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	m, err := s.upsertMonitorCheckinTx(ctx, tx, params)
	if err != nil {
		return err
	}
	for _, ch := range channels {
		if ch.OrgID != m.OrgID {
			return errors.New("monitor channel scope mismatch")
		}
		// A repeated failure in the same episode leaves a delivered row alone.
		// On a new failure after recovery, the latest episode replaces an
		// outstanding one. The old worker's generation and lease token can no
		// longer finish that row.
		_, err = tx.Exec(ctx, `
INSERT INTO monitor_failure_outbox
    (monitor_id,org_id,project_id,channel_id,failed_generation,observed_at)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (monitor_id,channel_id) DO UPDATE SET
    failed_generation=EXCLUDED.failed_generation,
    observed_at=EXCLUDED.observed_at,
    state='pending', attempts=0, next_attempt_at=now(),
    lease_token='', leased_until=NULL, last_error_code='', updated_at=now()
WHERE monitor_failure_outbox.failed_generation < EXCLUDED.failed_generation
   OR monitor_failure_outbox.state='cancelled'`, m.ID, m.OrgID, m.ProjectID,
			ch.ID, m.FailedGeneration, params.LastPingAt.Time)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type monitorFailureNotice struct {
	monitorID, orgID, projectID, channelID, leaseToken string
	generation                                         int64
	observedAt                                         time.Time
	attempts                                           int32
}

const monitorFailureHealthCap = 1001

// MonitorFailureOutboxHealth is a tenant-free, bounded readiness projection.
// Counts of 1001 mean at least 1001, so the probe never scans an unbounded
// backlog. Oldest age includes due notices and expired leases.
type MonitorFailureOutboxHealth struct {
	Pending, Due, ExpiredLeases, OldestDueSeconds int64
	CountsCapped                                  bool
}

func (s *Server) monitorFailureOutboxHealth(ctx context.Context) (MonitorFailureOutboxHealth, error) {
	if s.pool == nil {
		return MonitorFailureOutboxHealth{}, errors.New("monitor failure outbox unavailable")
	}
	var h MonitorFailureOutboxHealth
	err := s.pool.QueryRow(ctx, `WITH pending AS MATERIALIZED (
    SELECT next_attempt_at FROM monitor_failure_outbox
    WHERE state='pending' ORDER BY next_attempt_at LIMIT $1
), expired AS MATERIALIZED (
    SELECT leased_until FROM monitor_failure_outbox
    WHERE state='leased' AND leased_until<=now()
    ORDER BY leased_until LIMIT $1
)
SELECT (SELECT count(*) FROM pending),
       (SELECT count(*) FROM pending WHERE next_attempt_at<=now()),
       (SELECT count(*) FROM expired),
       COALESCE(EXTRACT(EPOCH FROM now()-LEAST(
           (SELECT min(next_attempt_at) FROM pending WHERE next_attempt_at<=now()),
           (SELECT min(leased_until) FROM expired))),0)::bigint`,
		monitorFailureHealthCap).Scan(&h.Pending, &h.Due, &h.ExpiredLeases, &h.OldestDueSeconds)
	if err != nil {
		return MonitorFailureOutboxHealth{}, fmt.Errorf("query monitor failure outbox health: %w", err)
	}
	h.CountsCapped = h.Pending >= monitorFailureHealthCap || h.Due >= monitorFailureHealthCap || h.ExpiredLeases >= monitorFailureHealthCap
	return h, nil
}

// claimMonitorFailureNotices is a global worker scan. SKIP LOCKED and a fresh
// token fence each attempt across replicas; a crashed worker's lease expires.
func (s *Server) claimMonitorFailureNotices(ctx context.Context) ([]monitorFailureNotice, error) {
	rows, err := s.pool.Query(ctx, `
WITH due AS (
    SELECT monitor_id, channel_id FROM monitor_failure_outbox
    WHERE (state='pending' AND next_attempt_at<=now())
       OR (state='leased' AND leased_until<=now())
    ORDER BY next_attempt_at, monitor_id, channel_id
    LIMIT 32 FOR UPDATE SKIP LOCKED
)
UPDATE monitor_failure_outbox o SET state='leased', lease_token=$1,
    leased_until=now()+interval '30 seconds', updated_at=now()
FROM due WHERE o.monitor_id=due.monitor_id AND o.channel_id=due.channel_id
RETURNING o.monitor_id,o.org_id,o.project_id,o.channel_id,
          o.failed_generation,o.observed_at,o.attempts,o.lease_token`, id.New())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []monitorFailureNotice
	for rows.Next() {
		var n monitorFailureNotice
		if err := rows.Scan(&n.monitorID, &n.orgID, &n.projectID, &n.channelID,
			&n.generation, &n.observedAt, &n.attempts, &n.leaseToken); err != nil {
			return nil, err
		}
		notices = append(notices, n)
	}
	return notices, rows.Err()
}

// currentMonitorFailureChannel rechecks tenant, project and current routing at
// the dispatch boundary. A recovered job still gets its accepted failure
// notice; a newer failure generation supersedes the old one.
func (s *Server) currentMonitorFailureChannel(ctx context.Context, n monitorFailureNotice) (alerts.Channel, string, string, error) {
	var ch alerts.Channel
	var slug, projectName string
	var config []byte
	err := s.pool.QueryRow(ctx, `
SELECT c.id,c.org_id,c.type,c.config,m.slug,p.name
FROM monitor_failure_outbox o
JOIN monitors m ON m.id=o.monitor_id AND m.org_id=o.org_id AND m.project_id=o.project_id
JOIN projects p ON p.id=o.project_id AND p.org_id=o.org_id
JOIN notification_channels c ON c.id=o.channel_id AND c.org_id=o.org_id
WHERE o.monitor_id=$1 AND o.channel_id=$2 AND o.org_id=$3 AND o.project_id=$4
  AND o.failed_generation=$5 AND o.state='leased' AND o.lease_token=$6
  AND c.enabled=true
  AND (EXISTS (SELECT 1 FROM channel_projects cp WHERE cp.channel_id=c.id AND cp.project_id=o.project_id)
       OR NOT EXISTS (SELECT 1 FROM channel_projects cp WHERE cp.channel_id=c.id))`,
		n.monitorID, n.channelID, n.orgID, n.projectID, n.generation, n.leaseToken).
		Scan(&ch.ID, &ch.OrgID, &ch.Type, &config, &slug, &projectName)
	if err != nil {
		return ch, "", "", err
	}
	ch.Config = s.decryptChannelConfig(ch.Type, config)
	return ch, slug, projectName, nil
}

func (s *Server) finishMonitorFailureNotice(ctx context.Context, n monitorFailureNotice, outcome string) error {
	var state, code string
	var delay time.Duration
	var countAttempt bool
	switch outcome {
	case "delivered":
		state = "delivered"
	case "cancelled":
		state, code = "cancelled", "route_removed"
	case "throttled":
		state, delay = "pending", time.Minute
	case "failed":
		state, code, countAttempt = "pending", "delivery_failed", true
		// Keep trying while routed, with bounded load and no terminal silent
		// state. Saturated channels can be repaired without losing the alert.
		shift := min(n.attempts, 8)
		delay = min(time.Hour, time.Duration(10*(1<<shift))*time.Second)
	default:
		return fmt.Errorf("invalid monitor notice outcome %q", outcome)
	}
	_, err := s.pool.Exec(ctx, `
UPDATE monitor_failure_outbox SET state=$7, attempts=attempts+$8,
    next_attempt_at=now()+$9::interval, lease_token='', leased_until=NULL,
    last_error_code=$10, updated_at=now()
WHERE monitor_id=$1 AND channel_id=$2 AND org_id=$3 AND project_id=$4
  AND failed_generation=$5 AND lease_token=$6 AND state='leased'`,
		n.monitorID, n.channelID, n.orgID, n.projectID, n.generation,
		n.leaseToken, state, boolToInt(countAttempt), fmt.Sprintf("%f seconds", delay.Seconds()), code)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RunMonitorFailureOutbox runs on every replica. A worker drop or process
// restart leaves the notice leased until it is reclaimed on another tick.
func (s *Server) RunMonitorFailureOutbox(ctx context.Context) {
	if s.pool == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		s.monitorFailureOutboxTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) monitorFailureOutboxTick(ctx context.Context) {
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	batch, err := s.claimMonitorFailureNotices(claimCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("claim failed monitor notices", "error", err)
		}
		return
	}
	for _, n := range batch {
		n := n
		// A saturated pool is safe: the lease expires and another tick retries.
		s.goBackgroundFor(n.orgID, "monitor-failed", 20*time.Second, func(jobCtx context.Context) {
			s.deliverMonitorFailureNotice(jobCtx, n)
		})
	}
}

func (s *Server) deliverMonitorFailureNotice(ctx context.Context, n monitorFailureNotice) {
	ch, slug, projectName, err := s.currentMonitorFailureChannel(ctx, n)
	outcome := "failed"
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		outcome = "cancelled"
	case err != nil:
		slog.Warn("load failed monitor channel", "monitor_id", n.monitorID, "error", err)
	case s.testLimiter != nil && !s.testLimiter.Allow("monitor-failed:"+n.orgID+":"+n.monitorID):
		outcome = "throttled"
	case s.monitorAlertLimiter != nil && !s.monitorAlertLimiter.Allow("monitor-failed-org:"+n.orgID):
		outcome = "throttled"
	default:
		if s.dispatcher == nil {
			break
		}
		err = s.dispatcher.DispatchOne(ctx, ch, alerts.Notification{
			ProjectID: n.projectID, ProjectName: projectName,
			Title: "Monitor failed: " + slug, Level: "warning",
			Reason:     "Check-in reported failure",
			URL:        strings.TrimRight(s.cfg.BaseURL, "/") + "/projects/" + n.projectID,
			ObservedAt: n.observedAt, AutomationEvent: "alert",
		})
		if err == nil {
			outcome = "delivered"
		} else {
			slog.Warn("failed monitor alert retry scheduled", "monitor_id", n.monitorID,
				"channel_id", n.channelID, "error", err)
		}
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.finishMonitorFailureNotice(finishCtx, n, outcome); err != nil {
		slog.Warn("finish failed monitor notice", "monitor_id", n.monitorID, "error", err)
	}
}
