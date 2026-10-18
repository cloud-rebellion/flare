-- name: ListMCPProjectsByOrg :many
-- MCP needs only safe project metadata, bounded in the database rather than
-- loading every project (including its public ingest credential) to truncate.
SELECT id,name,slug,platform
FROM projects
WHERE org_id=sqlc.arg(org_id)
ORDER BY created_at DESC,id ASC
LIMIT sqlc.arg(row_limit);

-- ciguard:allow-no-project bounded tenant-wide MCP overview aggregate
-- name: MCPOverviewProjects :many
-- Preserve the overview's actionable unresolved-only project rows and join
-- safe metadata without first materializing the tenant's whole project list.
SELECT p.id AS project_id,p.name,p.slug,count(i.id)::bigint AS unresolved
FROM issues i
JOIN projects p ON p.id=i.project_id AND p.org_id=i.org_id
WHERE i.org_id=sqlc.arg(org_id) AND i.status='unresolved'
  AND i.level NOT IN ('info','debug')
GROUP BY p.id,p.name,p.slug
ORDER BY unresolved DESC,p.id ASC
LIMIT sqlc.arg(row_limit);
