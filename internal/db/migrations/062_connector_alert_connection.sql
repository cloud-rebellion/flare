-- +goose Up
-- Connector alert rules may target one registered installation.  The empty
-- value preserves provider-wide matching; non-empty values are validated by
-- the API against the caller's organization before the rule is written.
ALTER TABLE alert_rules
    ADD COLUMN connector_connection_id TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_connector_connection_id_bounds
    CHECK (length(connector_connection_id) <= 128);

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_connector_connection_id_bounds;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS connector_connection_id;
