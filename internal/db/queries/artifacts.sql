-- name: UpsertSourceMap :one
INSERT INTO source_map_artifacts (id, project_id, org_id, release, name, content, parse_status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (project_id, release, name) DO UPDATE
  SET content = EXCLUDED.content, parse_status = EXCLUDED.parse_status, created_at = now()
RETURNING id, project_id, org_id, release, name, octet_length(content) AS size, parse_status, created_at;

-- name: ListSourceMapsByProject :many
SELECT id, release, name, octet_length(content) AS size, parse_status, created_at
FROM source_map_artifacts
WHERE project_id = $1 AND org_id = $2
ORDER BY release DESC, name ASC;

-- name: GetSourceMapsForRelease :many
-- Bounded by BYTES, not just by row count.
--
-- A row cap alone does not bound memory: maxArtifactBody allows a 30 MiB source
-- map, so LIMIT 1000 permits 30 GB in a single request. The cap has to be here
-- rather than in Go, because pgx materialises every row before the handler sees
-- one, so a Go-side budget would run after the allocation it is meant to
-- prevent.
--
-- The window function accumulates content size over the same ordering the LIMIT
-- uses, so this takes the newest maps until the budget is spent and then stops.
-- Symbolication already degrades gracefully when a map is absent: frames stay
-- minified rather than the request failing.
SELECT name, content FROM (
    SELECT name, content, created_at,
           sum(octet_length(content)) OVER (ORDER BY created_at DESC, name) AS running_bytes
    FROM source_map_artifacts
    WHERE project_id = $1 AND org_id = $2 AND release = $3
      AND parse_status IN ('unvalidated', 'valid')
) t
WHERE running_bytes <= sqlc.arg(max_bytes)::bigint
ORDER BY created_at DESC
LIMIT 1000;

-- name: DeleteSourceMap :execrows
DELETE FROM source_map_artifacts WHERE id = $1 AND project_id = $2 AND org_id = $3;

-- name: CountSourceMapsByProject :one
-- Row count and total bytes for a project's source maps, for the upload quota.
SELECT count(*)::bigint AS artifacts, coalesce(sum(octet_length(content)), 0)::bigint AS bytes
FROM source_map_artifacts WHERE project_id = $1 AND org_id = $2;
