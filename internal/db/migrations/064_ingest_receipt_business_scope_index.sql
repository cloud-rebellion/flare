-- +goose NO TRANSACTION
-- +goose Up
-- PostgreSQL needs a nonpartial unique index on the referenced receipt
-- columns for the business check's composite tenant/project foreign key.
-- Build it concurrently so existing receipt ingestion is not held behind
-- an index scan. Keep this nontransactional step separate from table DDL.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ingest_receipts_id_org_project_unique
    ON ingest_receipts (id, org_id, project_id);

-- A failed concurrent build can leave an INVALID index with this name. IF NOT
-- EXISTS would silently skip it on restart, so fail before migration 065.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = 'ingest_receipts_id_org_project_unique'::regclass
          AND indrelid = 'ingest_receipts'::regclass
          AND indisvalid AND indisready AND indisunique AND indpred IS NULL
    ) THEN
        RAISE EXCEPTION 'ingest_receipts_id_org_project_unique is invalid; drop it concurrently and rerun migration 064';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS ingest_receipts_id_org_project_unique;
