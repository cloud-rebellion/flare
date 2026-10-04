-- +goose NO TRANSACTION
-- +goose Up
-- A connector attempt may include both a project and an organization. The
-- project primary key alone cannot enforce that those two scopes agree. Build
-- the referenced key without blocking project writes during an index scan.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS projects_id_org_connector_attempts_key
    ON projects (id, org_id);

-- A failed concurrent build can leave an INVALID index. IF NOT EXISTS would
-- skip it on restart, so fail before migration 068 adds the foreign key.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = 'projects_id_org_connector_attempts_key'::regclass
          AND indrelid = 'projects'::regclass
          AND indisvalid AND indisready AND indisunique AND indpred IS NULL
    ) THEN
        RAISE EXCEPTION 'projects_id_org_connector_attempts_key is invalid; drop it concurrently and rerun migration 067';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS projects_id_org_connector_attempts_key;
