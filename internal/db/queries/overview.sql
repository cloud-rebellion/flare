-- Org-wide dashboard aggregates for the Overview page. Every query is scoped
-- by org_id (the tenant boundary); they intentionally span all of the org's
-- projects, so each carries a ciguard allow-no-project marker directly above
-- its -- name: directive.
--
-- The Overview is the ACTIONABLE ops view, so every metric here excludes
-- info/debug-level signals (heartbeats, breadcrumbs, health check-ins): they
-- are informational, never incidents, and must not inflate counts, dominate Top
-- Issues, or ride the volume charts. They still exist (visible via the project
-- issue list with an explicit filter) and still count toward the watchdog's
-- silence detection, which reads events directly and is unaffected by this.

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewEventCount24h :one
SELECT count(*) FROM events
WHERE org_id = $1 AND received_at >= now() - interval '24 hours'
  AND level NOT IN ('info', 'debug');

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewUnresolvedCount :one
SELECT count(*) FROM issues
WHERE org_id = $1 AND status = 'unresolved'
  AND level NOT IN ('info', 'debug');

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewNewIssuesToday :one
SELECT count(*) FROM issues
WHERE org_id = $1 AND first_seen >= date_trunc('day', now())
  AND level NOT IN ('info', 'debug');

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewEventVolumeByHour :many
SELECT date_trunc('hour', received_at)::timestamptz AS hour, count(*) AS count
FROM events
WHERE org_id = $1 AND received_at >= now() - interval '24 hours'
  AND level NOT IN ('info', 'debug')
GROUP BY hour
ORDER BY hour;

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewTopIssues :many
SELECT i.id, i.title, i.level, i.event_count, i.project_id, p.name AS project_name
FROM issues i
JOIN projects p ON p.id = i.project_id
WHERE i.org_id = $1 AND i.status = 'unresolved'
  AND i.level NOT IN ('info', 'debug')
ORDER BY i.event_count DESC, i.last_seen DESC
LIMIT 8;

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewProjectVolumeByHour :many
SELECT project_id, date_trunc('hour', received_at)::timestamptz AS hour, count(*) AS count
FROM events
WHERE org_id = $1 AND received_at >= now() - interval '24 hours'
  AND level NOT IN ('info', 'debug')
GROUP BY project_id, hour
ORDER BY project_id, hour;

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewProjectUnresolved :many
SELECT project_id, count(*) AS count
FROM issues
WHERE org_id = $1 AND status = 'unresolved'
  AND level NOT IN ('info', 'debug')
GROUP BY project_id;

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewProjectLastSeen :many
-- Liveness pulse: the newest signal of ANY level per project. This deliberately
-- does NOT exclude info/debug: an info-level heartbeat ("service-up:<svc>") is
-- exactly what proves a service is alive when it has no errors, so it must count
-- here even though it is filtered out of every actionable count above. Bounded to
-- the last 7 days so the scan stays on recent partitions; a project with no row
-- has had no pulse in a week and is treated as silent.
SELECT project_id, max(received_at)::timestamptz AS last_seen
FROM events
WHERE org_id = $1 AND received_at >= now() - interval '7 days'
GROUP BY project_id;

-- ciguard:allow-no-project org-wide dashboard aggregate (scoped by org_id)
-- name: OverviewIngestCoverage24h :many
-- Completed receipts are the acceptance evidence. Pending requests are not.
SELECT project_id, pillar,
       sum(accepted_count)::bigint AS accepted_count,
       sum(rejected_count)::bigint AS rejected_count,
       sum(dropped_count)::bigint AS dropped_count,
       max(received_at) FILTER (WHERE accepted_count > 0)::timestamptz AS last_accepted_at
FROM ingest_receipts
WHERE org_id = $1 AND received_at >= now() - interval '24 hours'
  AND completed_at IS NOT NULL
GROUP BY project_id, pillar;
