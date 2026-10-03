-- +goose Up

-- Log-severity rules count one canonical log level in a bounded watchdog
-- window. The selector is deliberately separate from event-level fields so a
-- log rule cannot silently change the meaning of an issue alert.
ALTER TABLE alert_rules
    ADD COLUMN log_severity TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_log_severity_valid
    CHECK (log_severity IN ('', 'trace', 'debug', 'info', 'warn', 'error', 'fatal'));

-- The watchdog query is bounded by tenant, project, severity, and time. The
-- logs table is partitioned, so adding a parent index at boot would lock every
-- partition; index maintenance belongs in a separately scheduled operation.

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_log_severity_valid;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS log_severity;
