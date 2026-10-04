-- +goose NO TRANSACTION
-- +goose Up
-- A connector attempt may include both a project and an organization. The
-- project primary key alone cannot enforce that those two scopes agree. Build
-- the referenced key without blocking project writes during an index scan.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS projects_id_org_connector_attempts_key
    ON projects (id, org_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS projects_id_org_connector_attempts_key;
