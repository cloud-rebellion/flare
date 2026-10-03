-- name: UpsertNativeSymbolArtifact :one
INSERT INTO native_symbol_artifacts (
    id, project_id, org_id, release, platform, format, name, digest, size, content, validation_status
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (project_id, release, platform, format, name) DO UPDATE
SET org_id = EXCLUDED.org_id,
    digest = EXCLUDED.digest,
    size = EXCLUDED.size,
    content = EXCLUDED.content,
    validation_status = EXCLUDED.validation_status,
    created_at = now()
RETURNING id, project_id, org_id, release, platform, format, name, digest, size, validation_status, created_at;

-- name: ListNativeSymbolArtifactsByProject :many
SELECT id, release, platform, format, name, digest, size, validation_status, created_at
FROM native_symbol_artifacts
WHERE project_id = $1 AND org_id = $2
ORDER BY release DESC, platform ASC, format ASC, name ASC
LIMIT $3;

-- name: CountNativeSymbolArtifactsByProject :one
SELECT count(*)::bigint AS artifacts,
       coalesce(sum(size), 0)::bigint AS bytes
FROM native_symbol_artifacts
WHERE project_id = $1 AND org_id = $2;

-- name: NativeSymbolArtifactExists :one
SELECT EXISTS (
    SELECT 1
    FROM native_symbol_artifacts
    WHERE project_id = $1 AND org_id = $2
      AND release = $3 AND platform = $4 AND format = $5 AND name = $6
) AS exists;

-- name: ListNativeSymbolArtifactMetadataForRelease :many
-- Read-path status is deliberately metadata-only. The symbol bytes remain in
-- the native artifact table and are never copied into issue, MCP, or AI
-- responses. The release and organization predicates are both required:
-- release strings are client supplied and are not a tenant boundary.
SELECT id, release, platform, format, name, digest, size, validation_status, created_at
FROM native_symbol_artifacts
WHERE project_id = sqlc.arg(project_id)
  AND org_id = sqlc.arg(org_id)
  AND release = sqlc.arg(release)
ORDER BY platform ASC, format ASC, name ASC
LIMIT sqlc.arg(row_limit);

-- name: DeleteNativeSymbolArtifact :execrows
DELETE FROM native_symbol_artifacts
WHERE id = $1 AND project_id = $2 AND org_id = $3;
