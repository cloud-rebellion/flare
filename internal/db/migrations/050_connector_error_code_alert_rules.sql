-- +goose Up

-- Connector error-rate rules may optionally select one fixed provider error
-- code (for example auth_expired). Empty keeps the existing all-errors
-- behavior. The selector is bounded to the same safe token shape as
-- connector_attempts.error_code and never carries provider response text.
ALTER TABLE alert_rules
    ADD COLUMN connector_error_code TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_connector_error_code_bounds
    CHECK (connector_error_code = '' OR connector_error_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$');

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_connector_error_code_bounds;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS connector_error_code;
