-- +goose Up

-- Metric threshold rules evaluate one tenant-owned metric series in the
-- watchdog window. Keep the rule configuration separate from the integer
-- event threshold used by the older rule types so decimal gauges remain exact.
ALTER TABLE alert_rules
    ADD COLUMN metric_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN comparison TEXT NOT NULL DEFAULT 'gte',
    ADD COLUMN threshold_value DOUBLE PRECISION NOT NULL DEFAULT 0;

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_comparison_valid
    CHECK (comparison IN ('gt', 'gte', 'lt', 'lte'));

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_comparison_valid;
ALTER TABLE alert_rules
    DROP COLUMN IF EXISTS threshold_value,
    DROP COLUMN IF EXISTS comparison,
    DROP COLUMN IF EXISTS metric_name;
