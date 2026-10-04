-- +goose Up
-- A registered installation belongs to one Flare project once an admin binds
-- it. Historical org-owned connections stay unbound until explicitly assigned;
-- their old receipts and alert rules are not relabeled by this migration.
SET LOCAL lock_timeout = '5s';
ALTER TABLE connector_connections ADD COLUMN project_id TEXT;
ALTER TABLE connector_connections
    ADD CONSTRAINT connector_connections_project_org_fk
    FOREIGN KEY (project_id, org_id)
    REFERENCES projects (id, org_id) ON DELETE SET NULL (project_id);
CREATE INDEX idx_connector_connections_project
    ON connector_connections (org_id, project_id, id)
    WHERE project_id IS NOT NULL;

-- A server receipt for a registered connection takes its project from the
-- connection row, not from an in-process caller. FOR SHARE serializes this
-- read with an admin's first binding or a connection delete. Existing receipt
-- rows are intentionally left untouched, including legacy_unknown provenance.
-- +goose StatementBegin
CREATE FUNCTION connector_attempt_bound_project() RETURNS trigger AS $$
DECLARE
    bound_project_id TEXT;
    bound_provider TEXT;
BEGIN
    IF NEW.connection_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT c.project_id, c.provider
      INTO bound_project_id, bound_provider
      FROM connector_connections c
     WHERE c.id = NEW.connection_id AND c.org_id = NEW.org_id
     FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'connector connection scope is invalid' USING ERRCODE = '23503';
    END IF;
    IF NEW.source_kind = 'server_observed' AND NEW.provider <> bound_provider THEN
        RAISE EXCEPTION 'connector provider does not match installation' USING ERRCODE = '23514';
    END IF;
    NEW.project_id := bound_project_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER connector_attempt_bound_project_before_insert
    BEFORE INSERT ON connector_attempts
    FOR EACH ROW EXECUTE FUNCTION connector_attempt_bound_project();

-- +goose Down
DROP TRIGGER IF EXISTS connector_attempt_bound_project_before_insert ON connector_attempts;
DROP FUNCTION IF EXISTS connector_attempt_bound_project();
DROP INDEX IF EXISTS idx_connector_connections_project;
ALTER TABLE connector_connections DROP CONSTRAINT IF EXISTS connector_connections_project_org_fk;
ALTER TABLE connector_connections DROP COLUMN IF EXISTS project_id;
