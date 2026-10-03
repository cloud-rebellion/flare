-- +goose Up

-- API-key hygiene rules alert on a bounded count of credentials that need
-- operator attention. The mode is fixed so a public API request cannot turn
-- the watchdog into an arbitrary credential query. Key material, prefixes,
-- names, and creator identities never cross the watchdog boundary.
ALTER TABLE alert_rules
    ADD COLUMN api_key_hygiene_mode TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_api_key_hygiene_mode_valid
    CHECK (api_key_hygiene_mode IN ('', 'unused', 'expiring'));

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_api_key_hygiene_mode_valid;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS api_key_hygiene_mode;
