-- Payload-free connector receipts. Never add URLs, request bodies, credentials,
-- or provider response text to this table or its API projection. A nullable
-- connection_id links first-party calls to one registered installation; legacy
-- and external DSN receipts remain null. source_kind is assigned by the
-- server's authentication boundary, never accepted from a sender body.

-- name: InsertConnectorAttempt :exec
INSERT INTO connector_attempts (
    id, org_id, project_id, connection_id, source_kind, provider, operation, channel, outcome,
    status_class, error_code, elapsed_ms, quota_remaining, retry_after_ms, sync_lag_ms,
    attempted_at, completed_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17);

-- name: ListConnectorHealthByOrg :many
SELECT source_kind, COALESCE(connection_id, '') AS connection_id,
       provider,
       operation,
       channel,
       count(*)::bigint AS attempts,
       count(*) FILTER (WHERE outcome = 'ok')::bigint AS ok_count,
       count(*) FILTER (WHERE outcome = 'error')::bigint AS error_count,
       avg(elapsed_ms)::double precision AS avg_elapsed_ms,
       max(elapsed_ms)::bigint AS max_elapsed_ms,
       max(attempted_at)::timestamptz AS last_attempted_at,
       max(completed_at)::timestamptz AS last_completed_at
FROM connector_attempts
WHERE org_id = $1
  AND completed_at >= $2
  AND source_kind = sqlc.arg(source_kind)::text
GROUP BY source_kind, connection_id, provider, operation, channel
ORDER BY last_completed_at DESC, connection_id, provider, operation, channel
LIMIT $3;

-- name: ListConnectorHealthByProject :many
SELECT source_kind, COALESCE(connection_id, '') AS connection_id,
       provider,
       operation,
       channel,
       count(*)::bigint AS attempts,
       count(*) FILTER (WHERE outcome = 'ok')::bigint AS ok_count,
       count(*) FILTER (WHERE outcome = 'error')::bigint AS error_count,
       avg(elapsed_ms)::double precision AS avg_elapsed_ms,
       max(elapsed_ms)::bigint AS max_elapsed_ms,
       max(attempted_at)::timestamptz AS last_attempted_at,
       max(completed_at)::timestamptz AS last_completed_at
FROM connector_attempts ca
WHERE ca.project_id = $1
  AND ca.org_id = $2
  AND ca.completed_at >= $3
  AND ca.source_kind = sqlc.arg(source_kind)::text
  AND (ca.connection_id IS NULL OR ca.source_kind <> 'server_observed' OR EXISTS (
      SELECT 1 FROM connector_connections c
      WHERE c.id = ca.connection_id AND c.org_id = ca.org_id AND c.project_id = ca.project_id
  ))
GROUP BY source_kind, connection_id, provider, operation, channel
ORDER BY last_completed_at DESC, connection_id, provider, operation, channel
LIMIT $4;

-- Quota/rate-limit/sync health is an aggregate projection of the latest bounded
-- provider hints. Each metric is latest-by-series, rather than max/min, so a
-- stale high quota or low lag value cannot hide a newer observation.
-- name: ListConnectorQuotaHealthByOrg :many
WITH series AS (
    SELECT COALESCE(connection_id, '') AS connection_id,
           provider,
           operation,
           channel,
           count(*) FILTER (WHERE quota_remaining IS NOT NULL)::bigint AS quota_points,
           count(*) FILTER (WHERE retry_after_ms IS NOT NULL)::bigint AS retry_points,
           count(*) FILTER (WHERE sync_lag_ms IS NOT NULL)::bigint AS sync_lag_points,
           max(completed_at)::timestamptz AS last_completed_at
    FROM connector_attempts ca
    WHERE ca.org_id = $1
      AND ca.source_kind = sqlc.arg(source_kind)::text
      AND ca.completed_at >= $2
      AND (ca.quota_remaining IS NOT NULL OR ca.retry_after_ms IS NOT NULL OR ca.sync_lag_ms IS NOT NULL)
    GROUP BY COALESCE(ca.connection_id, ''), ca.provider, ca.operation, ca.channel
), quota_latest AS (
    SELECT DISTINCT ON (COALESCE(connection_id, ''), provider, operation, channel)
           COALESCE(connection_id, '') AS connection_id, provider, operation, channel, quota_remaining,
           completed_at AS quota_observed_at
    FROM connector_attempts ca
    WHERE ca.org_id = $1
      AND ca.source_kind = sqlc.arg(source_kind)::text
      AND ca.completed_at >= $2
      AND ca.quota_remaining IS NOT NULL
    ORDER BY COALESCE(ca.connection_id, ''), ca.provider, ca.operation, ca.channel, ca.completed_at DESC, ca.id DESC
), retry_latest AS (
    SELECT DISTINCT ON (COALESCE(connection_id, ''), provider, operation, channel)
           COALESCE(connection_id, '') AS connection_id, provider, operation, channel, retry_after_ms,
           completed_at AS retry_observed_at
    FROM connector_attempts ca
    WHERE ca.org_id = $1
      AND ca.source_kind = sqlc.arg(source_kind)::text
      AND ca.completed_at >= $2
      AND ca.retry_after_ms IS NOT NULL
    ORDER BY COALESCE(ca.connection_id, ''), ca.provider, ca.operation, ca.channel, ca.completed_at DESC, ca.id DESC
), sync_lag_latest AS (
    SELECT DISTINCT ON (COALESCE(connection_id, ''), provider, operation, channel)
           COALESCE(connection_id, '') AS connection_id, provider, operation, channel, sync_lag_ms,
           completed_at AS sync_lag_observed_at
    FROM connector_attempts ca
    WHERE ca.org_id = $1
      AND ca.source_kind = sqlc.arg(source_kind)::text
      AND ca.completed_at >= $2
      AND ca.sync_lag_ms IS NOT NULL
    ORDER BY COALESCE(ca.connection_id, ''), ca.provider, ca.operation, ca.channel, ca.completed_at DESC, ca.id DESC
)
SELECT sqlc.arg(source_kind)::text AS source_kind,
       s.connection_id,
       s.provider,
       s.operation,
       s.channel,
       s.quota_points,
       s.retry_points,
       s.sync_lag_points,
       q.quota_remaining,
       q.quota_observed_at,
       rr.retry_after_ms,
       rr.retry_observed_at,
       sl.sync_lag_ms,
       sl.sync_lag_observed_at,
       s.last_completed_at
FROM series s
LEFT JOIN quota_latest q USING (connection_id, provider, operation, channel)
LEFT JOIN retry_latest rr USING (connection_id, provider, operation, channel)
LEFT JOIN sync_lag_latest sl USING (connection_id, provider, operation, channel)
ORDER BY s.last_completed_at DESC, s.connection_id, s.provider, s.operation, s.channel
LIMIT $3;

-- Project-scoped counterpart for app dashboards. Keep the same latest-by-series
-- semantics as the organization view, while requiring both project and org
-- ownership so a guessed project id cannot cross a tenant boundary.
-- name: ListConnectorQuotaHealthByProject :many
WITH series AS (
    SELECT COALESCE(connection_id, '') AS connection_id,
           provider,
           operation,
           channel,
           count(*) FILTER (WHERE quota_remaining IS NOT NULL)::bigint AS quota_points,
           count(*) FILTER (WHERE retry_after_ms IS NOT NULL)::bigint AS retry_points,
           count(*) FILTER (WHERE sync_lag_ms IS NOT NULL)::bigint AS sync_lag_points,
           max(completed_at)::timestamptz AS last_completed_at
    FROM connector_attempts ca
    WHERE ca.project_id = $1
      AND ca.org_id = $2
      AND ca.source_kind = sqlc.arg(source_kind)::text
      AND ca.completed_at >= $3
      AND (ca.connection_id IS NULL OR ca.source_kind <> 'server_observed' OR EXISTS (
          SELECT 1 FROM connector_connections c
          WHERE c.id = ca.connection_id AND c.org_id = ca.org_id AND c.project_id = ca.project_id
      ))
      AND (ca.quota_remaining IS NOT NULL OR ca.retry_after_ms IS NOT NULL OR ca.sync_lag_ms IS NOT NULL)
    GROUP BY COALESCE(ca.connection_id, ''), ca.provider, ca.operation, ca.channel
), quota_latest AS (
    SELECT DISTINCT ON (COALESCE(connection_id, ''), provider, operation, channel)
           COALESCE(connection_id, '') AS connection_id, provider, operation, channel, quota_remaining,
           completed_at AS quota_observed_at
    FROM connector_attempts ca
    WHERE ca.project_id = $1
      AND ca.org_id = $2
      AND ca.source_kind = sqlc.arg(source_kind)::text
      AND ca.completed_at >= $3
      AND (ca.connection_id IS NULL OR ca.source_kind <> 'server_observed' OR EXISTS (
          SELECT 1 FROM connector_connections c
          WHERE c.id = ca.connection_id AND c.org_id = ca.org_id AND c.project_id = ca.project_id
      ))
      AND ca.quota_remaining IS NOT NULL
    ORDER BY COALESCE(ca.connection_id, ''), ca.provider, ca.operation, ca.channel, ca.completed_at DESC, ca.id DESC
), retry_latest AS (
    SELECT DISTINCT ON (COALESCE(connection_id, ''), provider, operation, channel)
           COALESCE(connection_id, '') AS connection_id, provider, operation, channel, retry_after_ms,
           completed_at AS retry_observed_at
    FROM connector_attempts ca
    WHERE ca.project_id = $1
      AND ca.org_id = $2
      AND ca.source_kind = sqlc.arg(source_kind)::text
      AND ca.completed_at >= $3
      AND (ca.connection_id IS NULL OR ca.source_kind <> 'server_observed' OR EXISTS (
          SELECT 1 FROM connector_connections c
          WHERE c.id = ca.connection_id AND c.org_id = ca.org_id AND c.project_id = ca.project_id
      ))
      AND ca.retry_after_ms IS NOT NULL
    ORDER BY COALESCE(ca.connection_id, ''), ca.provider, ca.operation, ca.channel, ca.completed_at DESC, ca.id DESC
), sync_lag_latest AS (
    SELECT DISTINCT ON (COALESCE(connection_id, ''), provider, operation, channel)
           COALESCE(connection_id, '') AS connection_id, provider, operation, channel, sync_lag_ms,
           completed_at AS sync_lag_observed_at
    FROM connector_attempts ca
    WHERE ca.project_id = $1
      AND ca.org_id = $2
      AND ca.source_kind = sqlc.arg(source_kind)::text
      AND ca.completed_at >= $3
      AND (ca.connection_id IS NULL OR ca.source_kind <> 'server_observed' OR EXISTS (
          SELECT 1 FROM connector_connections c
          WHERE c.id = ca.connection_id AND c.org_id = ca.org_id AND c.project_id = ca.project_id
      ))
      AND ca.sync_lag_ms IS NOT NULL
    ORDER BY COALESCE(ca.connection_id, ''), ca.provider, ca.operation, ca.channel, ca.completed_at DESC, ca.id DESC
)
SELECT sqlc.arg(source_kind)::text AS source_kind,
       s.connection_id,
       s.provider,
       s.operation,
       s.channel,
       s.quota_points,
       s.retry_points,
       s.sync_lag_points,
       q.quota_remaining,
       q.quota_observed_at,
       rr.retry_after_ms,
       rr.retry_observed_at,
       sl.sync_lag_ms,
       sl.sync_lag_observed_at,
       s.last_completed_at
FROM series s
LEFT JOIN quota_latest q USING (connection_id, provider, operation, channel)
LEFT JOIN retry_latest rr USING (connection_id, provider, operation, channel)
LEFT JOIN sync_lag_latest sl USING (connection_id, provider, operation, channel)
ORDER BY s.last_completed_at DESC, s.connection_id, s.provider, s.operation, s.channel
LIMIT $4;

-- The watchdog selects one metric and one latest observation inside a bounded
-- tenant/project/provider series. Only fixed health fields cross this query
-- boundary.
-- name: GetConnectorQuotaStats :one
WITH observations AS (
    SELECT completed_at,
           CASE
               WHEN sqlc.arg(metric)::text = 'quota_remaining' THEN quota_remaining
               WHEN sqlc.arg(metric)::text = 'retry_after_ms' THEN retry_after_ms
               ELSE sync_lag_ms
           END AS value
    FROM connector_attempts ca
    WHERE COALESCE(project_id, '') = sqlc.arg(project_id)::text
      AND org_id = sqlc.arg(org_id)::text
      AND source_kind = sqlc.arg(source_kind)::text
      AND (ca.connection_id IS NULL OR ca.source_kind <> 'server_observed' OR EXISTS (
          SELECT 1 FROM connector_connections c
          WHERE c.id = ca.connection_id AND c.org_id = ca.org_id AND c.project_id = ca.project_id
      ))
      AND provider = sqlc.arg(provider)::text
      AND (sqlc.arg(operation)::text = '' OR operation = sqlc.arg(operation)::text)
      AND (sqlc.arg(channel)::text = '' OR channel = sqlc.arg(channel)::text)
      AND (sqlc.arg(connection_id)::text = '' OR connection_id = sqlc.arg(connection_id)::text)
      AND (sqlc.arg(connection_id)::text = '' OR EXISTS (
          SELECT 1 FROM connector_connections c
          WHERE c.id = sqlc.arg(connection_id)::text
            AND c.org_id = sqlc.arg(org_id)::text
            AND c.project_id = sqlc.arg(project_id)::text
      ))
      AND completed_at >= sqlc.arg(completed_at)
      AND (
          (sqlc.arg(metric)::text = 'quota_remaining' AND quota_remaining IS NOT NULL)
          OR (sqlc.arg(metric)::text = 'retry_after_ms' AND retry_after_ms IS NOT NULL)
          OR (sqlc.arg(metric)::text = 'sync_lag_ms' AND sync_lag_ms IS NOT NULL)
      )
)
SELECT count(*)::bigint AS points,
       COALESCE((
           SELECT value FROM observations
           ORDER BY completed_at DESC
           LIMIT 1
       ), 0)::bigint AS latest_value
FROM observations;
