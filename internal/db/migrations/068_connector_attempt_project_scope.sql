-- +goose Up
-- Enforce the paired project/organization scope on new connector receipts
-- immediately. NOT VALID leaves any older misattributed rows available for
-- the next migration to repair without scanning under this DDL lock.
-- Keep the metadata-only DDL from waiting indefinitely behind another DDL
-- holder; Goose rolls this whole migration back on a lock timeout and retries
-- it on the next startup.
SET LOCAL lock_timeout = '5s';
ALTER TABLE connector_attempts
    ADD CONSTRAINT connector_attempts_project_org_fk
    FOREIGN KEY (project_id, org_id)
    REFERENCES projects (id, org_id) ON DELETE SET NULL (project_id)
    NOT VALID;

-- +goose Down
ALTER TABLE connector_attempts
    DROP CONSTRAINT IF EXISTS connector_attempts_project_org_fk;
