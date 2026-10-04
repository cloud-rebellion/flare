-- +goose Up
-- Business health rules select one documented, bounded business check name.
-- The project route remains the notification scope; org_id is always carried
-- by the existing alert rule row and watchdog query.
ALTER TABLE alert_rules
    ADD COLUMN business_check_name TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_business_check_name_bound
    CHECK (business_check_name = '' OR business_check_name ~ '^[a-z][a-z0-9._:-]{0,63}$');

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_business_check_name_bound;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS business_check_name;
