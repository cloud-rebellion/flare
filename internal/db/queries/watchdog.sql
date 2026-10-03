-- Watchdog worker queries. The worker runs across all orgs/projects, so
-- ListWatchdogRules is intentionally not project-scoped; the per-project reads
-- it drives (CountEventsForProjectSince) carry the project_id + org_id predicate.

-- ciguard:allow-unscoped global watchdog worker; every downstream read (CountEventsForProjectSince, GetMetricThresholdStats, GetTracePercentileStats, GetServiceLevelStats, GetConnectorErrorRateStats, GetConnectorQuotaStats, GetConnectorFreshnessStats, GetConnectorSyncHealthStats, GetServiceFreshnessStats, GetReleaseErrorRateStats, GetReleaseFreshnessStats, GetDeployFailureStats, GetDeployAcceptanceStats, GetRollbackStats, GetLogSeverityStats, GetUsageThresholdStats, GetAPIKeyHygieneStats, GetAITriageStats, GetIngestAcceptanceStats) re-scopes by project_id/org_id or org_id for org-owned API keys, and TrySetAlertRuleFired filters org_id
-- name: ListWatchdogRules :many
-- Enabled watchdog rules with the project name + org for dispatch.
SELECT r.id, r.project_id, r.org_id, r.name, r.type, r.threshold, r.window_minutes,
       r.metric_name, r.comparison, r.threshold_value, r.span_name, r.percentile,
       r.connector_provider, r.connector_operation, r.connector_channel, r.connector_error_code, r.connector_connection_id, r.connector_sync_mode,
       r.release_version, r.service_name, r.log_severity, r.connector_quota_metric,
       r.usage_surface, r.usage_operation, r.usage_measure,
       r.api_key_hygiene_mode, r.ai_triage_mode, r.metric_labels,
       r.connector_freshness_baseline_at, r.service_freshness_baseline_at,
       r.created_at, r.last_fired_at, p.name AS project_name
FROM alert_rules r
JOIN projects p ON p.id = r.project_id
WHERE r.enabled = true AND r.type IN ('anomaly', 'silence', 'metric_threshold', 'trace_percentile', 'service_level', 'connector_error_rate', 'connector_freshness', 'connector_sync_health', 'service_freshness', 'release_freshness', 'connector_quota', 'release_error_rate', 'deploy_failure', 'deploy_acceptance', 'rollback', 'log_severity', 'usage_threshold', 'api_key_hygiene', 'ai_triage', 'ingest_acceptance');

-- Sync state is organization-owned because one registered installation may be
-- used by several project routes. The alert itself remains project-scoped for
-- notification routing, but the selector is always one exact connection and
-- operation. `failed` counts recent terminal failures; `overdue` counts rows
-- whose scheduled run or lease expiry is past the configured grace cutoff.
-- Paused/revoked connections are excluded so intentional disconnects do not
-- page. Only counts cross the watchdog boundary.
-- name: GetConnectorSyncHealthStats :one
SELECT count(*) FILTER (
           WHERE s.state = 'failed'
             AND s.updated_at >= sqlc.arg(cutoff)
       )::bigint AS failed_count,
       count(*) FILTER (
           WHERE (
               s.next_run_at IS NOT NULL
               AND s.next_run_at < sqlc.arg(cutoff)
           ) OR (
               s.state = 'running'
               AND s.lease_expires_at IS NOT NULL
               AND s.lease_expires_at < sqlc.arg(cutoff)
           )
       )::bigint AS overdue_count
FROM connector_sync_states s
JOIN connector_connections c
  ON c.id = s.connection_id
 AND c.org_id = s.org_id
WHERE s.org_id = sqlc.arg(org_id)::text
  AND s.connection_id = sqlc.arg(connection_id)::text
  AND c.provider = sqlc.arg(provider)::text
  AND s.operation = sqlc.arg(operation)::text
  AND c.status IN ('active', 'pending', 'error');

-- In-progress receipts are created before a telemetry handler touches the
-- payload. A process crash or lost completion write leaves one here forever,
-- making an HTTP 202/200 impossible to distinguish from durable acceptance.
-- Count only old, incomplete rows and return no body, request id, or payload.
-- The watchdog applies the project and org predicates so this remains a
-- tenant-safe aggregate even though the rule worker is global.
-- name: GetIngestAcceptanceStats :one
SELECT count(*)::bigint AS stale_count
FROM ingest_receipts
WHERE project_id = sqlc.arg(project_id)::text
  AND org_id = sqlc.arg(org_id)::text
  AND outcome = 'in_progress'
  AND completed_at IS NULL
  AND received_at < sqlc.arg(cutoff);

-- AI triage rules aggregate only safe status/error-code counts. Failed calls
-- are measured by their bounded start time; stuck calls are still in `started`
-- with no completion and older than the same cutoff. Prompts, completions,
-- issue content, provider details, and error text never cross this boundary.
-- name: GetAITriageStats :one
       SELECT count(*) FILTER (
           WHERE status = 'failed'
             AND completed_at IS NOT NULL
             AND started_at >= sqlc.arg(cutoff)
       )::bigint AS failed_count,
       count(*) FILTER (
           WHERE status = 'failed'
             AND completed_at IS NOT NULL
             AND started_at >= sqlc.arg(cutoff)
             AND error_code IS NOT NULL
       )::bigint AS failed_error_count,
       count(*) FILTER (
           WHERE status = 'started'
             AND completed_at IS NULL
             AND started_at < sqlc.arg(cutoff)
       )::bigint AS stuck_count
FROM ai_action_receipts
WHERE project_id = sqlc.arg(project_id)::text
  AND org_id = sqlc.arg(org_id)::text
  AND operation = 'triage'
  AND (
      (status = 'failed' AND completed_at IS NOT NULL AND started_at >= sqlc.arg(cutoff))
      OR (status = 'started' AND completed_at IS NULL AND started_at < sqlc.arg(cutoff))
  );

-- Deploy-acceptance rules count passed deploy receipts that have aged beyond
-- the configured grace period without a later passed synthetic receipt for the
-- same issue and target revision. The lookback is bounded by the watchdog so a
-- forgotten historical deploy cannot page forever. Only tenant-safe counts
-- cross this boundary; receipt references and issue content stay out.
-- name: GetDeployAcceptanceStats :one
SELECT count(*)::bigint AS acceptance_count
FROM automation_receipts d
WHERE d.project_id = sqlc.arg(project_id)::text
  AND d.org_id = sqlc.arg(org_id)::text
  AND d.stage = 'deploy'
  AND d.status = 'passed'
  AND d.completed_at IS NOT NULL
  AND d.completed_at >= sqlc.arg(since)
  AND d.completed_at < sqlc.arg(grace_cutoff)
  AND NOT EXISTS (
      SELECT 1
      FROM automation_receipts s
      WHERE s.project_id = d.project_id
        AND s.org_id = d.org_id
        AND s.issue_id = d.issue_id
        AND s.stage = 'synthetic'
        AND s.status = 'passed'
        AND s.target_revision = d.target_revision
        AND s.completed_at IS NOT NULL
        AND s.completed_at >= d.completed_at
  );

-- Connector-freshness rules use an explicit rule baseline and only completed
-- attempts after that baseline.  A missing row is intentional: the watchdog
-- falls back to the durable baseline, so a newly configured empty series stays
-- quiet until its cadence has elapsed.  Provider/operation/channel selectors
-- are all tenant/project scoped and no connector body crosses this query.
-- name: GetConnectorFreshnessStats :one
SELECT max(completed_at)::timestamptz AS last_completed_at
FROM connector_attempts
WHERE COALESCE(project_id, '') = sqlc.arg(project_id)::text
  AND org_id = sqlc.arg(org_id)::text
  AND provider = sqlc.arg(provider)::text
  AND (sqlc.arg(operation)::text = '' OR operation = sqlc.arg(operation)::text)
  AND (sqlc.arg(channel)::text = '' OR channel = sqlc.arg(channel)::text)
  AND (sqlc.arg(connection_id)::text = '' OR connection_id = sqlc.arg(connection_id)::text)
  AND completed_at >= sqlc.arg(baseline_at);

-- Service-freshness rules inspect only the exact canonical heartbeat for one
-- service/release dimension. A service sharing a release with another
-- component cannot keep this rule fresh. Tenant, project, release, and
-- bounded service selectors all participate in the predicate; no event body
-- or heartbeat content crosses the watchdog boundary.
-- name: GetServiceFreshnessStats :one
SELECT max(e.received_at)::timestamptz AS last_heartbeat_at
FROM events e
WHERE e.project_id = sqlc.arg(project_id)::text
  AND e.org_id = sqlc.arg(org_id)::text
  AND e.release = sqlc.arg(release_version)::text
  AND e.level = 'info'
  AND e.message = ('service-up:' || sqlc.arg(service_name)::text)
  AND e.received_at >= sqlc.arg(baseline_at);

-- Metric threshold rules inspect only aggregate values from one bounded,
-- tenant-scoped series. A normalized JSON object selector keeps stable metric
-- names independently alertable by environment, release, route, or method.
-- Min/max let the rule express both high-water and low-water alerts without
-- returning labels or raw points to the watchdog.
-- A zero point count is preserved so a low-water rule does not fire merely
-- because a series has never reported.
-- name: GetMetricThresholdStats :one
SELECT count(*)::bigint AS points,
       COALESCE(min(value), 0)::double precision AS min_value,
       COALESCE(max(value), 0)::double precision AS max_value
FROM metrics
WHERE project_id = $1
  AND org_id = $2
  AND name = $3
  AND COALESCE(labels, '{}'::jsonb) @> sqlc.arg(metric_labels)::jsonb
  AND observed_at >= sqlc.arg(observed_at);

-- Connector error-rate rules aggregate only the project and org selected by
-- the watchdog row. Empty operation/channel selectors intentionally match all
-- values, and an empty error-code selector intentionally matches all errors.
-- Provider is always required by the API. No response payloads or connector
-- credentials cross this query boundary.
-- name: GetConnectorErrorRateStats :one
SELECT count(*)::bigint AS attempts,
       count(*) FILTER (WHERE outcome = 'error')::bigint AS error_count
FROM connector_attempts
WHERE COALESCE(project_id, '') = sqlc.arg(project_id)::text
  AND org_id = sqlc.arg(org_id)::text
  AND provider = sqlc.arg(provider)::text
  AND (sqlc.arg(operation)::text = '' OR operation = sqlc.arg(operation)::text)
  AND (sqlc.arg(channel)::text = '' OR channel = sqlc.arg(channel)::text)
  AND (sqlc.arg(error_code)::text = '' OR error_code = sqlc.arg(error_code)::text)
  AND (sqlc.arg(connection_id)::text = '' OR connection_id = sqlc.arg(connection_id)::text)
  AND completed_at >= sqlc.arg(completed_at);

-- Deploy-failure rules count only failed or blocked deploy stages in the same
-- tenant and project. The receipt contract stores bounded stage/status/error
-- metadata, so this query never crosses source, provider, or issue payloads.
-- name: GetDeployFailureStats :one
SELECT count(*)::bigint AS failure_count
FROM automation_receipts
WHERE project_id = sqlc.arg(project_id)::text
  AND org_id = sqlc.arg(org_id)::text
  AND stage = 'deploy'
  AND status IN ('failed', 'blocked')
  AND created_at >= sqlc.arg(since);

-- Rollback rules count only explicit terminal rollback receipts. The dedicated
-- stage keeps an ordinary deploy failure from being mistaken for a rollback;
-- source/target revisions remain bounded opaque references on the receipt.
-- name: GetRollbackStats :one
SELECT count(*)::bigint AS rollback_count
FROM automation_receipts
WHERE project_id = sqlc.arg(project_id)::text
  AND org_id = sqlc.arg(org_id)::text
  AND stage = 'rollback'
  AND status IN ('passed', 'failed', 'blocked')
  AND created_at >= sqlc.arg(since);

-- Log-severity rules inspect only a count for one canonical severity in one
-- tenant and project. Bodies, attributes, trace IDs, and span IDs never cross
-- this watchdog boundary.
-- name: GetLogSeverityStats :one
SELECT count(*)::bigint AS log_count
FROM logs
WHERE project_id = sqlc.arg(project_id)
  AND org_id = sqlc.arg(org_id)
  AND (
      severity = sqlc.arg(severity)
      OR (sqlc.arg(severity)::text = 'warn' AND severity = 'warning')
      OR (sqlc.arg(severity)::text = 'fatal' AND severity = 'critical')
  )
  AND observed_at >= sqlc.arg(since);

-- Usage threshold rules aggregate only durable, payload-free one-minute
-- buckets. DSN rows are project scoped. API-key and MCP rows are org-wide and
-- use an empty project_id; those surfaces use the explicit org-wide branch
-- while preserving the owning project's tenant boundary.
-- name: GetUsageThresholdStats :one
SELECT count(*)::bigint AS bucket_count,
       COALESCE(sum(request_count), 0)::bigint AS request_count,
       COALESCE(sum(error_count), 0)::bigint AS error_count,
       COALESCE(sum(denied_count), 0)::bigint AS denied_count
FROM usage_buckets
WHERE org_id = sqlc.arg(org_id)::text
  AND surface = sqlc.arg(surface)::text
  AND operation = sqlc.arg(operation)::text
  AND bucket_start >= sqlc.arg(since)
  AND (
      project_id = sqlc.arg(project_id)::text
      OR (surface IN ('api_key', 'mcp') AND project_id = '')
  );

-- API-key hygiene rules aggregate only a count of org-owned credentials. The
-- selected project routes the notification, but API keys are organization
-- scoped by design, so the org predicate is the authoritative tenant fence.
-- `unused` counts keys that have never been used (or have been idle) and are
-- older than the configured horizon. `expiring` counts non-expired keys whose
-- expiry falls within the same horizon. No key names, prefixes, hashes, users, or expiry
-- timestamps leave the aggregate query.
-- name: GetAPIKeyHygieneStats :one
SELECT count(*)::bigint AS key_count
FROM api_keys
WHERE org_id = sqlc.arg(org_id)::text
  AND (
      (sqlc.arg(mode)::text = 'unused'
       AND (last_used_at IS NULL OR last_used_at <= sqlc.arg(older_than))
       AND created_at <= sqlc.arg(older_than))
      OR
      (sqlc.arg(mode)::text = 'expiring'
       AND expires_at IS NOT NULL
       AND expires_at > sqlc.arg(as_of)
       AND expires_at <= sqlc.arg(expires_before))
  );

-- Release error-rate rules inspect only aggregate event counts for one
-- project-owned release. An explicit release_version is checked against the
-- tenant/project release registry. An empty selector chooses the newest
-- release with either a deploy marker or event in the bounded watchdog window;
-- no raw event payload, issue text, or stacktrace crosses this boundary.
-- name: GetReleaseErrorRateStats :one
WITH release_candidates AS (
    SELECT r.version,
           r.created_at,
           max(e.received_at) AS last_seen
    FROM releases r
    LEFT JOIN events e
      ON e.project_id = r.project_id
     AND e.org_id = r.org_id
     AND e.release = r.version
     AND e.received_at >= sqlc.arg(since)
    WHERE r.project_id = sqlc.arg(project_id)
      AND r.org_id = sqlc.arg(org_id)
      AND (
        sqlc.arg(release_version)::text = ''
        OR r.version = sqlc.arg(release_version)::text
      )
    GROUP BY r.version, r.created_at
), selected_release AS (
    SELECT version
    FROM release_candidates
    WHERE sqlc.arg(release_version)::text <> ''
       OR created_at >= sqlc.arg(since)
       OR last_seen IS NOT NULL
    ORDER BY COALESCE(last_seen, created_at) DESC, version DESC
    LIMIT 1
)
SELECT COALESCE((SELECT version FROM selected_release), '')::text AS release_version,
       COALESCE((
           SELECT count(e.id)
           FROM events e
           JOIN selected_release s ON s.version = e.release
           WHERE e.project_id = sqlc.arg(project_id)
             AND e.org_id = sqlc.arg(org_id)
             AND e.received_at >= sqlc.arg(since)
       ), 0)::bigint AS event_count,
       COALESCE((
           SELECT count(e.id)
           FROM events e
           JOIN selected_release s ON s.version = e.release
           WHERE e.project_id = sqlc.arg(project_id)
             AND e.org_id = sqlc.arg(org_id)
             AND e.received_at >= sqlc.arg(since)
             AND e.level NOT IN ('info', 'debug')
       ), 0)::bigint AS error_count;

-- Release-freshness rules require an explicit release selector and an explicit
-- deploy marker. The row is absent for releases that were only auto-created by
-- event traffic. Age is measured from the last canonical service-up heartbeat,
-- or from the deploy marker until the first heartbeat arrives.
-- name: GetReleaseFreshnessStats :many
SELECT r.version,
       r.deploy_marked_at,
       r.last_heartbeat_at,
       EXTRACT(EPOCH FROM (now() - COALESCE(r.last_heartbeat_at, r.deploy_marked_at)))::double precision AS age_seconds
FROM releases r
WHERE r.project_id = sqlc.arg(project_id)::text
  AND r.org_id = sqlc.arg(org_id)::text
  AND r.version = sqlc.arg(release_version)::text
  AND r.deploy_marked_at IS NOT NULL
LIMIT 1;

-- Trace percentile rules inspect one bounded span operation.  The count is
-- preserved so a newly-created rule never fires before that operation has
-- reported a span.  percentile_cont is computed inside PostgreSQL over the
-- tenant and project predicate; no span names, attributes, or raw rows leave
-- the database.
-- name: GetTracePercentileStats :one
SELECT count(*)::bigint AS points,
       COALESCE(
           percentile_cont(sqlc.arg(percentile)::double precision)
           WITHIN GROUP (ORDER BY duration_ms),
           0
       )::double precision AS percentile_value
FROM spans
WHERE project_id = sqlc.arg(project_id)
  AND org_id = sqlc.arg(org_id)
  AND name = sqlc.arg(span_name)
  AND start_time >= sqlc.arg(since);

-- Service-level rules evaluate the observed availability of one exact span
-- operation against a latency objective.  The watchdog stores the latency
-- objective in the existing integer threshold column and the target in the
-- existing decimal threshold_value column, so this adds no unbounded rule
-- fields or migration.  A missing series remains quiet; service freshness or
-- a check-in should page when the producer stops reporting altogether.
-- name: GetServiceLevelStats :one
SELECT count(*)::bigint AS span_count,
       count(*) FILTER (
           WHERE status NOT ILIKE '%error%'
             AND duration_ms <= sqlc.arg(latency_ms)::double precision
       )::bigint AS good_count
FROM spans
WHERE project_id = sqlc.arg(project_id)
  AND org_id = sqlc.arg(org_id)
  AND name = sqlc.arg(span_name)
  AND start_time >= sqlc.arg(since);

-- name: CountEventsForProjectSince :one
-- EVERY event, including info/debug. This is the SILENCE count, and it must stay
-- unfiltered: the info-level "service-up" heartbeat is precisely the signal that
-- proves a healthy, low-error service is still alive.
SELECT count(*) FROM events
WHERE project_id = $1 AND org_id = $2 AND received_at >= $3;

-- name: CountActionableEventsForProjectSince :one
-- Only ACTIONABLE events. This is the ANOMALY count.
--
-- An "error-rate anomaly" must not fire on heartbeat volume. Both rules used to
-- share the unfiltered count above, so a project going from silent to
-- heartbeating looked like an infinite error spike against its own zero
-- baseline: the entire estate tripped this rule on 2026-07-11 (the day the
-- heartbeat shipped), and mesh-hub + mesh-cloud tripped it again the moment they
-- were first instrumented. Heartbeats are liveness, not errors.
SELECT count(*) FROM events
WHERE project_id = $1 AND org_id = $2 AND received_at >= $3
  AND level NOT IN ('info', 'debug');

-- name: TrySetAlertRuleFired :execrows
-- Test-and-set cooldown: claims the alert for this rule only when it has not
-- fired since $3 (now - cooldown). Returns 1 when claimed, 0 when still cooling.
UPDATE alert_rules SET last_fired_at = now()
WHERE id = $1 AND org_id = $2 AND (last_fired_at IS NULL OR last_fired_at < $3);
