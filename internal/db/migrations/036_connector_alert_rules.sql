-- +goose Up

-- Connector error-rate rules select one bounded project-owned connector
-- series. Empty operation/channel values mean "all" for that dimension, which
-- lets one rule cover a provider without introducing unbounded label filters.
ALTER TABLE alert_rules
    ADD COLUMN connector_provider TEXT NOT NULL DEFAULT '',
    ADD COLUMN connector_operation TEXT NOT NULL DEFAULT '',
    ADD COLUMN connector_channel TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_connector_selector_bounds
    CHECK (length(connector_provider) <= 32
       AND length(connector_operation) <= 64
       AND length(connector_channel) <= 32);

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_connector_selector_bounds;
ALTER TABLE alert_rules
    DROP COLUMN IF EXISTS connector_channel,
    DROP COLUMN IF EXISTS connector_operation,
    DROP COLUMN IF EXISTS connector_provider;
