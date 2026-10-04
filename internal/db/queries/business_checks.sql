-- Business check names are caller-defined bounded tokens, while outcome,
-- elapsed time, and safe error/status classes are the entire stored signal.
-- No URL, body, customer identifier, credential, or provider text is stored.

-- name: InsertBusinessCheck :one
INSERT INTO business_checks (
    id, org_id, project_id, receipt_id, business_key_id, idempotency_key_hash, payload_hash, check_name, outcome,
    status_class, error_code, elapsed_ms, completed_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (org_id, project_id, idempotency_key_hash) DO NOTHING
RETURNING receipt_id;

-- name: GetBusinessCheckByIdempotencyKey :one
SELECT receipt_id, payload_hash
FROM business_checks
WHERE org_id = sqlc.arg(org_id)::text
  AND project_id = sqlc.arg(project_id)::text
  AND idempotency_key_hash = sqlc.arg(idempotency_key_hash)::text;

-- name: ListBusinessHealthByProject :many
SELECT check_name,
       count(*)::bigint AS attempts,
       count(*) FILTER (WHERE outcome = 'ok')::bigint AS ok_count,
       count(*) FILTER (WHERE outcome = 'error')::bigint AS error_count,
       avg(elapsed_ms)::double precision AS avg_elapsed_ms,
       max(elapsed_ms)::bigint AS max_elapsed_ms,
       max(completed_at)::timestamptz AS last_completed_at,
       max(completed_at) FILTER (WHERE outcome = 'ok')::timestamptz AS last_ok_at,
       max(completed_at) FILTER (WHERE outcome = 'error')::timestamptz AS last_error_at
FROM business_checks
WHERE project_id = sqlc.arg(project_id)::text
  AND org_id = sqlc.arg(org_id)::text
  AND completed_at >= sqlc.arg(since)
GROUP BY check_name
ORDER BY last_completed_at DESC, check_name
LIMIT sqlc.arg(row_limit);

-- name: GetBusinessHealthStats :one
SELECT count(*)::bigint AS attempts,
       count(*) FILTER (WHERE outcome = 'error')::bigint AS error_count
FROM business_checks
WHERE project_id = sqlc.arg(project_id)::text
  AND org_id = sqlc.arg(org_id)::text
  AND check_name = sqlc.arg(check_name)::text
  AND completed_at >= sqlc.arg(since);
