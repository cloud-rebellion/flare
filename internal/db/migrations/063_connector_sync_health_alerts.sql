-- +goose Up
-- A sync-health rule is project-routed but watches one registered,
-- organization-owned installation and one durable sync operation. Empty is
-- retained for older rules; the API requires a value for the new rule type.
ALTER TABLE alert_rules
    ADD COLUMN connector_sync_mode TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_connector_sync_mode_valid
    CHECK (connector_sync_mode IN ('', 'failed', 'overdue'));

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_connector_sync_mode_valid;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS connector_sync_mode;
