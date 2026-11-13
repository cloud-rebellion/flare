-- name: CreateOrg :one
-- The first org on an instance becomes the operator workspace (migration
-- 093); every later one is a tenant. Only an EMPTY instance hands out the
-- flag: orgs with no operator is a broken state a human repairs, never a
-- prize for the next registrant. Callers hold LockOperatorOrg.
INSERT INTO orgs (id, name, slug, is_operator)
VALUES ($1, $2, $3, NOT EXISTS (SELECT 1 FROM orgs))
RETURNING *;

-- name: LockOperatorOrg :exec
-- Serialises everything that decides who the operator workspace is: org
-- creation and org deletion. Without it a registration committing inside an
-- operator delete saw the operator still present and became a tenant, and the
-- instance was left with orgs but no operator.
SELECT pg_advisory_xact_lock(hashtext('flare.operator_org'));

-- name: OperatorOrgMissing :one
-- True when workspaces exist but none is the operator (checked at boot).
SELECT EXISTS (SELECT 1 FROM orgs) AND NOT EXISTS (SELECT 1 FROM orgs WHERE is_operator);

-- name: GetOrgBySlug :one
SELECT * FROM orgs WHERE slug = $1;

-- name: GetOrgByID :one
SELECT * FROM orgs WHERE id = $1;

-- name: CountOrgs :one
SELECT count(*) FROM orgs;

-- name: DeleteOrg :exec
-- Right-to-erasure: cascades to users, projects (and their telemetry),
-- channels, keys, invites, github_config and audit_log via ON DELETE CASCADE.
DELETE FROM orgs WHERE id = $1;

-- ciguard:allow-unscoped the scs session table is keyed by opaque token with no org column; the caller matches on the encoded user ids it is erasing
-- name: DeleteSessionsForUsers :exec
-- The sessions table is the one store holding tenant data that no foreign key
-- reaches: scs writes an opaque token plus a gob blob, so an org delete used to
-- leave every one of its members' live session rows behind. Matching on the
-- encoded user id inside the payload is the only handle available.
DELETE FROM sessions
WHERE EXISTS (
    SELECT 1 FROM unnest(@user_ids::text[]) AS u(id)
    WHERE position(u.id in encode(data, 'escape')) > 0
);

-- name: GetFirstOrg :one
-- The system/owner scope for Flare's own security events (e.g. an ingest-auth
-- rejection that matches no project has no org): the operator workspace, or
-- the oldest org if none is flagged. id breaks a created_at tie so the answer
-- never depends on the plan.
SELECT * FROM orgs ORDER BY is_operator DESC, created_at ASC, id ASC LIMIT 1;

-- name: GetOperatorOrg :one
-- The operator workspace, which alone may manage instance-wide settings.
-- No row means no operator, and callers refuse (never fall back to "oldest").
SELECT * FROM orgs WHERE is_operator;
