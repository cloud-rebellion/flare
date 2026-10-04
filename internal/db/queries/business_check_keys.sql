-- Business keys are independent from the public DSN and scoped to one project.
-- The bearer secret is shown exactly once; only its hash is stored.

-- name: LockProjectForBusinessKeyCreation :one
-- Serializes active-key cap checks for concurrent administrators of one project.
SELECT id FROM projects WHERE id = $1 AND org_id = $2 FOR UPDATE;

-- name: CountActiveBusinessCheckKeys :one
SELECT count(*)::bigint FROM business_check_keys
WHERE org_id = $1 AND project_id = $2
  AND revoked_at IS NULL AND expires_at > now();

-- name: CreateBusinessCheckKey :one
INSERT INTO business_check_keys (
    id, org_id, project_id, name, key_hash, key_prefix, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListBusinessCheckKeys :many
SELECT id, name, key_prefix, created_at, expires_at, revoked_at
FROM business_check_keys
WHERE org_id = $1 AND project_id = $2
-- Put every usable key ahead of revoked/expired history, even after 100
-- historical keys have accumulated.
ORDER BY (revoked_at IS NULL AND expires_at > now()) DESC, created_at DESC, id DESC
LIMIT 100;

-- name: RevokeBusinessCheckKey :execrows
UPDATE business_check_keys SET revoked_at = now()
WHERE id = $1 AND org_id = $2 AND project_id = $3 AND revoked_at IS NULL;

-- ciguard:allow-unscoped authentication path keyed by a 192-bit bearer secret hash; tenant and project come from the matching key and path
-- name: GetBusinessCheckKeyForIngest :one
SELECT k.id AS business_key_id, p.id AS project_id, p.org_id
FROM business_check_keys AS k
JOIN projects AS p ON p.id = k.project_id AND p.org_id = k.org_id
WHERE k.key_hash = $1
  AND (p.id = $2 OR p.dsn_id = $2)
  AND k.revoked_at IS NULL
  AND k.expires_at > now();

-- name: LockActiveBusinessCheckKey :one
-- This row lock conflicts with revocation's UPDATE. The acceptance transaction
-- must acquire it before inserting the receipt, so revoke and acceptance have
-- a single database order even if the first auth lookup raced with revoke.
SELECT id FROM business_check_keys
WHERE id = $1 AND org_id = $2 AND project_id = $3
  AND revoked_at IS NULL AND expires_at > now()
FOR SHARE;
