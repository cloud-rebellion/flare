// Package automationoutbox persists bounded, payload-free automation notices.
// A notice identifies the original alert and destination, but never stores
// channel credentials or user-controlled issue content. Delivery is at least
// once; consumers must deduplicate the stable delivery ID.
package automationoutbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/automationdelivery"
	"github.com/bright-interaction/flare/internal/id"
)

const (
	MaxAttempts = 12
	LeaseTime   = 30 * time.Second
	BatchSize   = 12
)

var (
	ErrRouteUnavailable       = errors.New("automation channel is no longer enabled or routed to this project")
	ErrStaleLease             = errors.New("automation delivery lease is no longer current")
	ErrNoticeIdentityConflict = errors.New("automation notice ID already belongs to different immutable fields")
)

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type Notice struct {
	ID          string
	OrgID       string
	ProjectID   string
	ChannelID   string
	IssueID     string
	Event       string
	EventCount  int64
	ObservedAt  time.Time
	Attempts    int
	LeaseToken  string
	LeasedUntil time.Time
}

// EnqueueTx must share the transaction that claims a watchdog rule, monitor,
// or spike. If the route changed after the caller loaded it, the whole claim
// rolls back and can be retried on a later tick.
func (s *Store) EnqueueTx(ctx context.Context, tx pgx.Tx, n Notice) (string, error) {
	if n.OrgID == "" || n.ProjectID == "" || n.ChannelID == "" || n.ObservedAt.IsZero() {
		return "", errors.New("automation notice requires tenant, channel, and observation time")
	}
	if n.ID == "" {
		n.ID = id.New()
	}
	n.ObservedAt = n.ObservedAt.UTC().Truncate(time.Microsecond)
	var inserted string
	err := tx.QueryRow(ctx, `
INSERT INTO automation_alert_outbox
    (id, org_id, project_id, channel_id, issue_id, event, event_count, observed_at)
SELECT $1, $2, $3, c.id, $5, $6, $7, $8
FROM notification_channels c
JOIN projects p ON p.id = $3 AND p.org_id = $2
WHERE c.id = $4 AND c.org_id = $2 AND c.type = 'automation_webhook' AND c.enabled
  AND (EXISTS (SELECT 1 FROM channel_projects cp WHERE cp.channel_id = c.id AND cp.project_id = $3)
       OR NOT EXISTS (SELECT 1 FROM channel_projects cp WHERE cp.channel_id = c.id))
ON CONFLICT (id) DO NOTHING
RETURNING id`, n.ID, n.OrgID, n.ProjectID, n.ChannelID, n.IssueID, n.Event, n.EventCount, n.ObservedAt.UTC()).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		// A caller-supplied stable ID may be retried after a lost response. An
		// exact replay is idempotent; any reused ID with altered tenant, route,
		// issue, event, count, or observation time is rejected.
		var existing Notice
		var channel *string
		lookupErr := tx.QueryRow(ctx, `SELECT org_id,project_id,channel_id,issue_id,event,event_count,observed_at
			FROM automation_alert_outbox WHERE id=$1`, n.ID).Scan(
			&existing.OrgID, &existing.ProjectID, &channel, &existing.IssueID,
			&existing.Event, &existing.EventCount, &existing.ObservedAt)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return "", ErrRouteUnavailable
		}
		if lookupErr != nil {
			return "", fmt.Errorf("read automation notice replay: %w", lookupErr)
		}
		if channel != nil {
			existing.ChannelID = *channel
		}
		if existing.OrgID != n.OrgID || existing.ProjectID != n.ProjectID ||
			existing.ChannelID != n.ChannelID || existing.IssueID != n.IssueID ||
			existing.Event != n.Event || existing.EventCount != n.EventCount ||
			!existing.ObservedAt.Equal(n.ObservedAt) {
			return "", ErrNoticeIdentityConflict
		}
		return n.ID, nil
	}
	if err != nil {
		return "", fmt.Errorf("enqueue automation notice: %w", err)
	}
	return inserted, nil
}

// ClaimDue fences a bounded set of due notices across replicas. A crashed
// worker's lease becomes eligible again, preserving the original notice ID.
// SKIP LOCKED prevents one slow worker from blocking other workers.
func (s *Store) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]Notice, error) {
	if limit < 1 || limit > BatchSize {
		limit = BatchSize
	}
	if lease <= 0 || lease > time.Minute {
		lease = LeaseTime
	}
	// A worker can crash after its final attempted send. Preserve that visible
	// failed row rather than leaving it leased forever or retrying past the cap.
	if _, err := s.pool.Exec(ctx, `
UPDATE automation_alert_outbox
SET state='dead', lease_token='', leased_until=NULL, last_error_code='delivery_failed', updated_at=now()
WHERE state='leased' AND leased_until < now() AND attempts >= $1`, MaxAttempts); err != nil {
		return nil, fmt.Errorf("expire exhausted automation leases: %w", err)
	}
	token := id.New()
	rows, err := s.pool.Query(ctx, `
WITH due AS (
    SELECT id FROM automation_alert_outbox
    WHERE (state='pending' AND next_attempt_at <= now())
       OR (state='leased' AND leased_until < now() AND attempts < $1)
    ORDER BY next_attempt_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE automation_alert_outbox o
SET state='leased', attempts=o.attempts+1, lease_token=$3,
    leased_until=now()+($4::bigint * interval '1 microsecond'), updated_at=now()
FROM due WHERE o.id=due.id
RETURNING o.id, o.org_id, o.project_id, o.channel_id, o.issue_id,
          o.event, o.event_count, o.observed_at, o.attempts, o.lease_token, o.leased_until`,
		MaxAttempts, limit, token, lease.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("claim automation notices: %w", err)
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		var channel *string
		if err := rows.Scan(&n.ID, &n.OrgID, &n.ProjectID, &channel, &n.IssueID,
			&n.Event, &n.EventCount, &n.ObservedAt, &n.Attempts, &n.LeaseToken, &n.LeasedUntil); err != nil {
			return nil, err
		}
		if channel != nil {
			n.ChannelID = *channel
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CurrentChannel rechecks tenant identity, channel type, enabled state, and
// project routing just before egress. Config is read fresh so rotation applies
// to queued notices. A missing row means the notice must be cancelled.
func (s *Store) CurrentChannel(ctx context.Context, n Notice) ([]byte, error) {
	if n.ChannelID == "" {
		return nil, pgx.ErrNoRows
	}
	var config []byte
	err := s.pool.QueryRow(ctx, `
SELECT c.config FROM notification_channels c
JOIN projects p ON p.id=$2 AND p.org_id=$1
WHERE c.id=$3 AND c.org_id=$1 AND c.type='automation_webhook' AND c.enabled
  AND (EXISTS (SELECT 1 FROM channel_projects cp WHERE cp.channel_id=c.id AND cp.project_id=$2)
       OR NOT EXISTS (SELECT 1 FROM channel_projects cp WHERE cp.channel_id=c.id))`,
		n.OrgID, n.ProjectID, n.ChannelID).Scan(&config)
	return config, err
}

// Finish succeeds only for the current, unexpired lease. An old worker can
// have sent a duplicate after expiry, but cannot overwrite a newer outcome.
func (s *Store) Finish(ctx context.Context, n Notice, delivered bool, cancelled bool, ack *automationdelivery.Acknowledgement) error {
	mode, runID, digest := "", "", ""
	var httpStatus any
	var deduped *bool
	if delivered {
		body, err := automationdelivery.NoticeBody(n.ProjectID, n.IssueID, n.Event, n.EventCount, n.ObservedAt)
		if cancelled || err != nil || ack == nil || !ack.Valid(n.Event, automationdelivery.Digest(body)) {
			return automationdelivery.ErrReceipt
		}
		mode, runID, digest, httpStatus, deduped = ack.Mode, ack.RunID, ack.RequestSHA256, ack.HTTPStatus, ack.Deduped
	} else if ack != nil {
		return automationdelivery.ErrReceipt
	}
	state := "pending"
	code := "delivery_failed"
	if delivered {
		state, code = "delivered", ""
	} else if cancelled {
		state, code = "cancelled", "route_removed"
	} else if n.Attempts >= MaxAttempts {
		state = "dead"
	}
	backoff := retryDelay(n.Attempts)
	result, err := s.pool.Exec(ctx, `
UPDATE automation_alert_outbox
SET state=$5, last_error_code=$6, lease_token='', leased_until=NULL,
    delivered_at=CASE WHEN $7::boolean THEN now() ELSE delivered_at END,
    next_attempt_at=CASE WHEN $5='pending' THEN now()+($8::bigint * interval '1 microsecond') ELSE next_attempt_at END,
    receiver_mode=$9, receiver_http_status=$10, receiver_run_id=$11,
    receiver_request_sha256=$12, receiver_deduped=$13,
    receiver_acknowledged_at=CASE WHEN $7::boolean THEN now() ELSE NULL END,
    updated_at=now()
WHERE id=$1 AND org_id=$2 AND project_id=$3 AND lease_token=$4
  AND state='leased' AND leased_until > now()
  AND issue_id=$14 AND event=$15 AND event_count=$16 AND observed_at=$17`,
		n.ID, n.OrgID, n.ProjectID, n.LeaseToken, state, code, delivered, backoff.Microseconds(),
		mode, httpStatus, runID, digest, deduped, n.IssueID, n.Event, n.EventCount, n.ObservedAt)
	if err != nil {
		return fmt.Errorf("finish automation notice: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrStaleLease
	}
	return nil
}

// DeliveryReceipt is a bounded read of original notice identity and delivery
// acknowledgement. No lease, destination, credential or receiver text is read.
type DeliveryReceipt struct {
	NoticeID        string     `json:"notice_id"`
	ChannelID       *string    `json:"channel_id,omitempty"`
	IssueID         string     `json:"issue_id"`
	Event           string     `json:"event"`
	EventCount      int64      `json:"event_count"`
	ObservedAt      time.Time  `json:"observed_at"`
	State           string     `json:"state"`
	Attempts        int        `json:"attempts"`
	Acknowledgement string     `json:"acknowledgement"`
	HTTPStatus      *int       `json:"http_status,omitempty"`
	RequestSHA256   string     `json:"request_sha256,omitempty"`
	ReceiverRunID   string     `json:"receiver_run_id,omitempty"`
	ReceiverDeduped *bool      `json:"receiver_deduped,omitempty"`
	AcknowledgedAt  *time.Time `json:"acknowledged_at,omitempty"`
}

func (s *Store) GetDeliveryReceipt(ctx context.Context, orgID, projectID, noticeID string) (DeliveryReceipt, error) {
	return scanDeliveryReceipt(s.pool.QueryRow(ctx, `SELECT id,channel_id,issue_id,event,event_count,observed_at,state,attempts,
        receiver_mode,receiver_http_status,receiver_request_sha256,receiver_run_id,receiver_deduped,receiver_acknowledged_at
        FROM automation_alert_outbox WHERE org_id=$1 AND project_id=$2 AND id=$3`, orgID, projectID, noticeID))
}

func scanDeliveryReceipt(row interface{ Scan(...any) error }, extra ...any) (DeliveryReceipt, error) {
	var r DeliveryReceipt
	var mode string
	fields := []any{
		&r.NoticeID, &r.ChannelID, &r.IssueID, &r.Event, &r.EventCount, &r.ObservedAt, &r.State, &r.Attempts,
		&mode, &r.HTTPStatus, &r.RequestSHA256, &r.ReceiverRunID, &r.ReceiverDeduped, &r.AcknowledgedAt}
	if err := row.Scan(append(fields, extra...)...); err != nil {
		return r, err
	}
	r.Acknowledgement = "unrecorded"
	if mode == automationdelivery.HTTP {
		r.Acknowledgement = "http_delivery"
	} else if mode == automationdelivery.ReactorV1 {
		if r.Event == "test" {
			r.Acknowledgement = "test_noop"
		} else {
			r.Acknowledgement = "receiver_enqueued"
		}
	}
	return r, nil
}

func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 10 {
		attempts = 10
	}
	return time.Duration(1<<attempts) * time.Second
}

type Stats struct {
	Pending int64
	Due     int64
	Leased  int64
	Dead    int64
	Oldest  *time.Time
}

// Snapshot is a payload-free, tenant-aggregate operational projection. The
// partial indexes avoid scanning delivered history on each worker tick.
func (s *Store) Snapshot(ctx context.Context) (Stats, error) {
	var out Stats
	err := s.pool.QueryRow(ctx, `
SELECT
    (SELECT count(*) FROM automation_alert_outbox WHERE state='pending'),
    (SELECT count(*) FROM automation_alert_outbox WHERE state='pending' AND next_attempt_at <= now()),
    (SELECT count(*) FROM automation_alert_outbox WHERE state='leased'),
    (SELECT count(*) FROM automation_alert_outbox WHERE state='dead'),
	    (SELECT min(next_attempt_at) FROM automation_alert_outbox WHERE state='pending' AND next_attempt_at <= now())`).Scan(
		&out.Pending, &out.Due, &out.Leased, &out.Dead, &out.Oldest)
	return out, err
}
