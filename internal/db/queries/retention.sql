-- retention_settings is instance-wide (one row, no tenant column): the
-- telemetry partitions and the cold tier it governs are shared by every org.
-- Writes are gated to admins of the operator workspace in the API layer.

-- ciguard:allow-unscoped retention_settings is one instance-wide row with no tenant; the users join only resolves who last changed it, for the operator-workspace admin view that the API gates
-- name: GetRetentionSettings :one
SELECT r.hot_retention_days, r.cold_mode, r.cold_retention_days, r.updated_at,
       u.email AS updated_by_email
FROM retention_settings r
LEFT JOIN users u ON u.id = r.updated_by
WHERE r.singleton;

-- name: UpsertRetentionSettings :exec
INSERT INTO retention_settings (singleton, hot_retention_days, cold_mode, cold_retention_days, updated_by, updated_at)
VALUES (TRUE, $1, $2, $3, $4, now())
ON CONFLICT (singleton) DO UPDATE
SET hot_retention_days = EXCLUDED.hot_retention_days,
    cold_mode = EXCLUDED.cold_mode,
    cold_retention_days = EXCLUDED.cold_retention_days,
    updated_by = EXCLUDED.updated_by,
    updated_at = now();

-- name: DeleteRetentionSettings :execrows
DELETE FROM retention_settings WHERE singleton;

-- Every policy write runs in one transaction that first takes this lock, so
-- two admins saving at once are serialised even when no row exists yet (a
-- row lock alone cannot cover "nothing stored"). Readers never take it.
-- name: LockRetentionSettings :exec
SELECT pg_advisory_xact_lock(hashtext('flare.retention_settings'));

-- ciguard:allow-unscoped same instance-wide row as GetRetentionSettings, read under the write lock so the confirm gate and the audit "from" see what the write replaces
-- name: GetRetentionSettingsForUpdate :one
SELECT r.hot_retention_days, r.cold_mode, r.cold_retention_days, r.updated_at,
       u.email AS updated_by_email
FROM retention_settings r
LEFT JOIN users u ON u.id = r.updated_by
WHERE r.singleton
FOR UPDATE OF r;
