-- Provider-agnostic connector synchronization state. Cursor ciphertext is
-- intentionally selected only by the worker boundary; public projections use
-- cursor_configured and cursor_version and never return cursor material.

-- name: EnsureConnectorSyncState :one
INSERT INTO connector_sync_states (id, org_id, connection_id, operation)
VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, connection_id, operation)
DO UPDATE SET updated_at = connector_sync_states.updated_at
RETURNING id, org_id, connection_id, operation, state,
          (cursor_ciphertext <> '') AS cursor_configured, cursor_version,
          attempt_count, last_started_at, last_completed_at, last_success_at,
          last_error_code, last_sync_lag_ms, next_run_at,
          lease_owner <> '' AS lease_active, lease_expires_at,
          created_at, updated_at;

-- name: GetConnectorSyncState :one
SELECT id, org_id, connection_id, operation, state,
       (cursor_ciphertext <> '') AS cursor_configured, cursor_version,
       attempt_count, last_started_at, last_completed_at, last_success_at,
       last_error_code, last_sync_lag_ms, next_run_at,
       lease_owner <> '' AS lease_active, lease_expires_at,
       created_at, updated_at
FROM connector_sync_states
WHERE org_id = $1 AND connection_id = $2 AND operation = $3;

-- name: GetConnectorSyncCursor :one
SELECT cursor_ciphertext, cursor_version
FROM connector_sync_states
WHERE org_id = $1 AND connection_id = $2 AND operation = $3;

-- name: ListConnectorSyncHealthByOrg :many
SELECT s.id, s.connection_id, c.provider, c.installation_id, c.installation_name,
       c.status AS connection_status, s.operation, s.state,
       (s.cursor_ciphertext <> '') AS cursor_configured, s.cursor_version,
       s.attempt_count, s.last_started_at, s.last_completed_at,
       s.last_success_at, s.last_error_code, s.last_sync_lag_ms,
       s.next_run_at, s.lease_owner <> '' AS lease_active,
       s.lease_expires_at, s.updated_at
FROM connector_sync_states s
JOIN connector_connections c
  ON c.id = s.connection_id AND c.org_id = s.org_id
WHERE s.org_id = $1
ORDER BY s.updated_at DESC, s.id DESC
LIMIT $2;

-- name: ListConnectorSyncHealthByProject :many
SELECT s.id, s.connection_id, c.provider, c.installation_id, c.installation_name,
       c.status AS connection_status, s.operation, s.state,
       (s.cursor_ciphertext <> '') AS cursor_configured, s.cursor_version,
       s.attempt_count, s.last_started_at, s.last_completed_at,
       s.last_success_at, s.last_error_code, s.last_sync_lag_ms,
       s.next_run_at, s.lease_owner <> '' AS lease_active,
       s.lease_expires_at, s.updated_at
FROM connector_sync_states s
JOIN connector_connections c
  ON c.id = s.connection_id AND c.org_id = s.org_id
WHERE c.project_id = $1 AND s.org_id = $2
ORDER BY s.updated_at DESC, s.id DESC
LIMIT $3;

-- The built-in scheduler intentionally scans only active HubSpot installations
-- with a configured credential. A missing state is due immediately; a stored
-- next_run_at controls the cadence, while an expired lease is due for recovery.
-- This is a server-owned cross-tenant scan; the worker receives org_id and
-- connection_id from this projection and every subsequent query binds both.
-- ciguard:allow-unscoped global opt-in connector scheduler; worker claim and
-- completion remain organization and connection scoped.
-- name: ListDueHubSpotConnectorConnections :many
SELECT c.org_id, c.id AS connection_id, c.provider,
       COALESCE(s.next_run_at, c.created_at) AS next_run_at
FROM connector_connections c
LEFT JOIN connector_sync_states s
  ON s.org_id = c.org_id
 AND s.connection_id = c.id
 AND s.operation = 'account_details'
WHERE c.provider = 'hubspot'
  AND c.status = 'active'
  AND c.secret_ciphertext <> ''
  AND (
      s.id IS NULL
      OR (
          (s.lease_expires_at IS NULL OR s.lease_expires_at <= sqlc.arg(now)::timestamptz)
          AND (s.next_run_at IS NULL OR s.next_run_at <= sqlc.arg(now)::timestamptz)
      )
  )
ORDER BY COALESCE(s.next_run_at, c.created_at) ASC, c.org_id ASC, c.id ASC
LIMIT sqlc.arg(batch_limit)::int;

-- Claim is atomic and only succeeds for an active connection. An unexpired
-- lease owned by another worker returns no row. The same worker may renew its
-- own lease, which makes retries safe after a transient observation timeout.
-- name: TryClaimConnectorSync :one
UPDATE connector_sync_states s
SET state = 'running',
    lease_owner = $4,
    lease_expires_at = $5,
    last_started_at = now(),
    attempt_count = s.attempt_count + 1,
    updated_at = now()
FROM connector_connections c
WHERE s.org_id = $1
  AND s.connection_id = $2
  AND s.operation = $3
  AND c.id = s.connection_id
  AND c.org_id = s.org_id
  AND c.status = 'active'
  AND (s.lease_expires_at IS NULL OR s.lease_expires_at <= now() OR s.lease_owner = $4)
RETURNING s.id, s.org_id, s.connection_id, s.operation, s.state,
          (s.cursor_ciphertext <> '') AS cursor_configured, s.cursor_version,
          s.attempt_count, s.last_started_at, s.last_completed_at,
          s.last_success_at, s.last_error_code, s.last_sync_lag_ms,
          s.next_run_at, s.lease_owner <> '' AS lease_active,
          s.lease_expires_at, s.created_at, s.updated_at;

-- Scheduled claims are due-gated in the same atomic UPDATE as the lease. A
-- caller cannot turn a due-list race into an early provider call, and a live
-- lease remains unavailable to another scheduler replica.
-- name: TryClaimDueConnectorSync :one
UPDATE connector_sync_states s
SET state = 'running',
    lease_owner = $4,
    lease_expires_at = $5,
    last_started_at = now(),
    attempt_count = s.attempt_count + 1,
    updated_at = now()
FROM connector_connections c
WHERE s.org_id = $1
  AND s.connection_id = $2
  AND s.operation = $3
  AND c.id = s.connection_id
  AND c.org_id = s.org_id
  AND c.status = 'active'
  AND (s.lease_expires_at IS NULL OR s.lease_expires_at <= now())
  AND (s.next_run_at IS NULL OR s.next_run_at <= now())
RETURNING s.id, s.org_id, s.connection_id, s.operation, s.state,
          (s.cursor_ciphertext <> '') AS cursor_configured, s.cursor_version,
          s.attempt_count, s.last_started_at, s.last_completed_at,
          s.last_success_at, s.last_error_code, s.last_sync_lag_ms,
          s.next_run_at, s.lease_owner <> '' AS lease_active,
          s.lease_expires_at, s.created_at, s.updated_at;

-- Completion is owner-guarded. A worker can advance a cursor only while it
-- owns the lease it claimed. Outcome is deliberately a fixed vocabulary and
-- all other values are recorded as failed by the SQL state transition.
-- name: CompleteConnectorSync :one
UPDATE connector_sync_states
SET state = CASE WHEN sqlc.arg(outcome)::text = 'ok' THEN 'succeeded' ELSE 'failed' END,
    cursor_ciphertext = CASE WHEN sqlc.arg(replace_cursor)::boolean THEN sqlc.arg(cursor_ciphertext)::text ELSE cursor_ciphertext END,
    cursor_version = CASE WHEN sqlc.arg(replace_cursor)::boolean THEN cursor_version + 1 ELSE cursor_version END,
    last_completed_at = now(),
    last_success_at = CASE WHEN sqlc.arg(outcome)::text = 'ok' THEN now() ELSE last_success_at END,
    last_error_code = CASE WHEN sqlc.arg(outcome)::text = 'ok' THEN '' ELSE sqlc.arg(last_error_code)::text END,
    last_sync_lag_ms = sqlc.narg(last_sync_lag_ms)::bigint,
    next_run_at = sqlc.narg(next_run_at)::timestamptz,
    lease_owner = '',
    lease_expires_at = NULL,
    updated_at = now()
WHERE org_id = sqlc.arg(org_id)::text
  AND connection_id = sqlc.arg(connection_id)::text
  AND operation = sqlc.arg(operation)::text
  AND lease_owner = sqlc.arg(lease_owner)::text
RETURNING id, org_id, connection_id, operation, state,
          (cursor_ciphertext <> '') AS cursor_configured, cursor_version,
          attempt_count, last_started_at, last_completed_at,
          last_success_at, last_error_code, last_sync_lag_ms,
          next_run_at, lease_owner <> '' AS lease_active,
          lease_expires_at, created_at, updated_at;
