-- name: CreateAPIKey :one
INSERT INTO api_keys (id, org_id, name, key_hash, key_prefix, expires_at, role, created_by_user_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: CreateMCPAPIKey :one
-- Database time binds expiry and creation atomically despite application clock
-- skew. The existing CreateAPIKey query omits purpose and keeps its API default.
INSERT INTO api_keys (id,org_id,name,key_hash,key_prefix,expires_at,role,created_by_user_id,purpose)
VALUES (sqlc.arg(id),sqlc.arg(org_id),sqlc.arg(name),sqlc.arg(key_hash),sqlc.arg(key_prefix),
        now() + (sqlc.arg(expires_in_days)::integer * interval '1 day'),
        sqlc.arg(role),sqlc.narg(created_by_user_id),'mcp')
RETURNING *;

-- ciguard:allow-unscoped auth path keyed by the secret key_hash; org_id is the result, not the filter
-- name: GetAPIKeyByHash :one
SELECT * FROM api_keys WHERE key_hash = $1;

-- ciguard:allow-unscoped touches a single row already resolved by key_hash above
-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1;

-- name: ListAPIKeysByOrg :many
-- Joins the creator's email so the settings UI can show who minted each key.
-- A NULL creator means the key predates migration 026: a password reset will NOT
-- revoke it, so an operator needs to see which keys are in that state and rotate
-- them deliberately.
SELECT k.*, u.email AS created_by_email
FROM api_keys k
LEFT JOIN users u ON u.id = k.created_by_user_id
WHERE k.org_id = $1
ORDER BY k.created_at DESC;

-- name: DeleteAPIKey :execrows
DELETE FROM api_keys WHERE id = $1 AND org_id = $2;

-- name: DeleteAPIKeysCreatedBy :execrows
-- Revoke every key a given user minted. Used by the password-reset path: a
-- reset must not leave behind a credential the attacker created while they had
-- the account. Keys with a NULL creator predate the column and are left alone
-- rather than guessed at.
-- The org_id predicate is redundant in principle (a user belongs to one org, so
-- their keys are already that org's) and explicit on purpose: ciguard refused
-- this query without it, correctly. A tenant-scoped table gets a tenant
-- predicate, and "it cannot happen" is how cross-tenant reads get written.
DELETE FROM api_keys
WHERE created_by_user_id = $1
  AND org_id = (SELECT org_id FROM users WHERE id = $1);
