-- +goose Up
-- Project alert rules for bounded durable usage counters.  DSN usage is
-- project-scoped; API-key and MCP usage buckets are org-wide and are evaluated
-- against the owning project's organization with an explicit query branch.
ALTER TABLE alert_rules
    ADD COLUMN usage_surface TEXT NOT NULL DEFAULT '',
    ADD COLUMN usage_operation TEXT NOT NULL DEFAULT '',
    ADD COLUMN usage_measure TEXT NOT NULL DEFAULT 'requests';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_usage_surface_valid
    CHECK (usage_surface IN ('', 'dsn', 'api_key', 'mcp'));

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_usage_operation_valid
    CHECK (length(usage_operation) <= 64);

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_usage_measure_valid
    CHECK (usage_measure IN ('requests', 'errors', 'denied'));

-- +goose Down
ALTER TABLE alert_rules
    DROP CONSTRAINT IF EXISTS alert_rules_usage_measure_valid,
    DROP CONSTRAINT IF EXISTS alert_rules_usage_operation_valid,
    DROP CONSTRAINT IF EXISTS alert_rules_usage_surface_valid;
ALTER TABLE alert_rules
    DROP COLUMN IF EXISTS usage_measure,
    DROP COLUMN IF EXISTS usage_operation,
    DROP COLUMN IF EXISTS usage_surface;
