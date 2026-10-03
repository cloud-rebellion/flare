-- +goose Up

-- Trace percentile rules evaluate one tenant-owned span operation in the
-- watchdog window.  The existing metric_name/comparison/threshold_value
-- columns remain dedicated to metric_threshold rules; keeping trace-specific
-- fields separate prevents one rule type from silently changing another's
-- meaning.
ALTER TABLE alert_rules
    ADD COLUMN span_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN percentile INTEGER NOT NULL DEFAULT 95;

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_percentile_valid
    CHECK (percentile BETWEEN 1 AND 99);

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_percentile_valid;
ALTER TABLE alert_rules
    DROP COLUMN IF EXISTS percentile,
    DROP COLUMN IF EXISTS span_name;
