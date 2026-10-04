-- +goose Up
-- Historical rows cannot be assigned a trustworthy origin retroactively.
-- Keep them queryable as legacy_unknown, but never treat them as observations
-- made by Flare's own provider adapters.
SET LOCAL lock_timeout = '5s';
ALTER TABLE connector_attempts
    ADD COLUMN source_kind TEXT NOT NULL DEFAULT 'legacy_unknown'
        CHECK (source_kind IN ('legacy_unknown', 'client_reported', 'server_observed'));
ALTER TABLE connector_attempts
    ADD CONSTRAINT connector_client_report_unbound
    CHECK (source_kind <> 'client_reported' OR connection_id IS NULL);

-- Existing rules previously mixed public DSN reports with server observations.
-- Bind them to the trusted server observation series after this migration.
-- Operators can create a client_reported rule explicitly for app observations.
ALTER TABLE alert_rules
    ADD COLUMN connector_source_kind TEXT NOT NULL DEFAULT ''
        CHECK (connector_source_kind IN ('', 'client_reported', 'server_observed'));
UPDATE alert_rules
SET connector_source_kind = 'server_observed'
WHERE type IN ('connector_error_rate', 'connector_quota', 'connector_freshness');
ALTER TABLE alert_rules
    ADD CONSTRAINT alert_connector_client_report_unbound
    CHECK (connector_source_kind <> 'client_reported' OR connector_connection_id = '');

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_connector_client_report_unbound;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS connector_source_kind;
ALTER TABLE connector_attempts DROP CONSTRAINT IF EXISTS connector_client_report_unbound;
ALTER TABLE connector_attempts DROP COLUMN IF EXISTS source_kind;
