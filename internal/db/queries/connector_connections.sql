-- Connector connection projections intentionally omit secret_ciphertext. The
-- only query that reads it is GetConnectorConnectionSecret, reserved for the
-- future provider worker at the outbound credential boundary.

-- name: CreateConnectorConnection :one
INSERT INTO connector_connections (
    id, org_id, provider, installation_id, installation_name, status, scopes, secret_ciphertext, project_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, sqlc.narg(project_id)::text)
RETURNING id, org_id, project_id, provider, installation_id, installation_name, status, scopes,
          (secret_ciphertext <> '') AS credential_configured, created_at, updated_at;

-- name: ListConnectorConnectionsByOrg :many
SELECT id, org_id, project_id, provider, installation_id, installation_name, status, scopes,
       (secret_ciphertext <> '') AS credential_configured, created_at, updated_at
FROM connector_connections
WHERE org_id = $1
ORDER BY updated_at DESC, id DESC;

-- name: GetConnectorConnectionByID :one
SELECT id, org_id, project_id, provider, installation_id, installation_name, status, scopes,
       (secret_ciphertext <> '') AS credential_configured, created_at, updated_at
FROM connector_connections
WHERE id = $1 AND org_id = $2;

-- name: GetConnectorConnectionSecret :one
SELECT secret_ciphertext
FROM connector_connections
WHERE id = $1 AND org_id = $2;

-- name: UpdateConnectorConnection :one
UPDATE connector_connections
SET provider = $3,
    installation_id = $4,
    installation_name = $5,
    status = $6,
    scopes = $7,
    secret_ciphertext = CASE WHEN sqlc.arg(replace_secret)::boolean THEN $8 ELSE secret_ciphertext END,
    project_id = CASE WHEN sqlc.arg(bind_project)::boolean THEN sqlc.narg(project_id)::text ELSE project_id END,
    updated_at = now()
WHERE id = $1 AND org_id = $2
  AND provider = $3 AND installation_id = $4
  AND (NOT sqlc.arg(bind_project)::boolean OR project_id IS NULL OR project_id = sqlc.narg(project_id)::text)
RETURNING id, org_id, project_id, provider, installation_id, installation_name, status, scopes,
          (secret_ciphertext <> '') AS credential_configured, created_at, updated_at;

-- name: DeleteConnectorConnection :execrows
DELETE FROM connector_connections
WHERE id = $1 AND org_id = $2;
