-- +goose Up
-- Preserve which registered installation produced a first-party connector
-- receipt. External DSN submissions and legacy rows remain nullable; the
-- composite foreign key prevents an app from attributing a receipt to another
-- tenant's connection.
ALTER TABLE connector_attempts
    ADD COLUMN connection_id TEXT;
ALTER TABLE connector_attempts
    ADD CONSTRAINT connector_attempts_connection_org_fk
    FOREIGN KEY (connection_id, org_id)
    REFERENCES connector_connections (id, org_id) ON DELETE SET NULL (connection_id);
CREATE INDEX idx_connector_attempts_connection_completed
    ON connector_attempts (org_id, connection_id, completed_at DESC)
    WHERE connection_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_connector_attempts_connection_completed;
ALTER TABLE connector_attempts
    DROP CONSTRAINT IF EXISTS connector_attempts_connection_org_fk;
ALTER TABLE connector_attempts
    DROP COLUMN IF EXISTS connection_id;
