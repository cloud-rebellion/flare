-- name: UpsertMonitorCheckin :one
-- Records a check-in. Creates the monitor on first ping with an unconfigured
-- interval, otherwise refreshes the last ping. The caller passes the computed
-- state so a successful ping clears a prior missing or failed state.
INSERT INTO monitors (id, org_id, project_id, slug, last_ping_at, last_status, state, failed_generation)
VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $7::text='failed' THEN 1 ELSE 0 END)
ON CONFLICT (project_id, slug) DO UPDATE
SET last_ping_at = EXCLUDED.last_ping_at,
    last_status = EXCLUDED.last_status,
    failed_generation = monitors.failed_generation + CASE
        WHEN EXCLUDED.state='failed' AND monitors.state<>'failed' THEN 1 ELSE 0 END,
    state = EXCLUDED.state
RETURNING *;

-- name: CountMonitorsByProject :one
SELECT count(*) FROM monitors WHERE project_id = $1 AND org_id = $2;

-- name: PruneUnconfiguredMonitors :execrows
-- Deletes auto-created monitors (interval_seconds = 0, i.e. never configured by
-- a human) that have not pinged inside the window. A configured monitor is
-- never touched: its whole purpose is to alert when it STOPS pinging.
DELETE FROM monitors
WHERE project_id = $1 AND org_id = $2
  AND interval_seconds = 0
  AND (last_ping_at IS NULL OR last_ping_at < $3);

-- name: GetMonitorBySlug :one
SELECT * FROM monitors WHERE project_id = $1 AND org_id = $2 AND slug = $3;

-- name: ListMonitorsByProject :many
SELECT * FROM monitors WHERE project_id = $1 AND org_id = $2 ORDER BY slug;

-- name: CreateMonitor :one
INSERT INTO monitors (id, org_id, project_id, slug, name, interval_seconds, grace_seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateMonitorConfig :one
UPDATE monitors
SET name = $3, interval_seconds = $4, grace_seconds = $5
WHERE id = $1 AND org_id = $2
RETURNING *;

-- name: DeleteMonitor :execrows
DELETE FROM monitors WHERE id = $1 AND org_id = $2;

-- ciguard:allow-unscoped global watchdog worker scanning every org's monitors; the claim it drives (TrySetMonitorMissing) filters id AND org_id
-- name: ListDueMonitors :many
-- Configured monitors whose last ping is older than interval plus grace.
-- Overdue is measured from COALESCE(last_ping_at, created_at): a monitor that
-- has NEVER checked in is the most important case (the scheduled job never ran
-- at all), and the previous `last_ping_at IS NOT NULL` guard excluded exactly
-- that case, so a cron that never fired once was never alerted.
-- Already-missing monitors are re-listed once past the re-notify window so a
-- dropped delivery cannot permanently suppress a dead-cron page.
SELECT m.*, p.name AS project_name
FROM monitors m
JOIN projects p ON p.id = m.project_id
WHERE m.interval_seconds > 0
  AND EXTRACT(EPOCH FROM ($1::timestamptz - COALESCE(m.last_ping_at, m.created_at)))
      > (m.interval_seconds + m.grace_seconds)
  AND (m.state <> 'missing'
       OR m.last_alert_at IS NULL
       OR EXTRACT(EPOCH FROM ($1::timestamptz - m.last_alert_at)) > sqlc.arg(renotify_seconds)::bigint);

-- name: TrySetMonitorMissing :execrows
-- Atomic claim: exactly one replica's UPDATE affects a row, so the alert fires
-- once per re-notify window. Mirrors ListDueMonitors' COALESCE and re-notify
-- predicates; if they drift apart the watchdog either double-pages or goes mute.
UPDATE monitors
SET state = 'missing', last_alert_at = $3
WHERE id = $1 AND org_id = $2
  -- Must mirror ListDueMonitors exactly, interval_seconds > 0 included: an
  -- unconfigured monitor (interval 0) is auto-created on first check-in and is
  -- not overdue by definition, so claiming it here would page for a monitor the
  -- listing query would never surface.
  AND interval_seconds > 0
  AND EXTRACT(EPOCH FROM ($3::timestamptz - COALESCE(last_ping_at, created_at)))
      > (interval_seconds + grace_seconds)
  AND (state <> 'missing'
       OR last_alert_at IS NULL
       OR EXTRACT(EPOCH FROM ($3::timestamptz - last_alert_at)) > sqlc.arg(renotify_seconds)::bigint);
