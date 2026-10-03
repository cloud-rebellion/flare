-- +goose Up
-- Bounded operational state for the cold-tier exporter. This ledger is
-- intentionally global (it contains no org/project identifiers and no
-- provider payloads); it records only the latest attempt for each partition.
-- Existing rows represent successful exports created before this telemetry was
-- added, so they retain the succeeded/default values with unknown byte size.
ALTER TABLE export_log
    ADD COLUMN status TEXT NOT NULL DEFAULT 'succeeded',
    ADD COLUMN attempted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN exported_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN bytes_known BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN export_latency_ms BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN error_code TEXT NOT NULL DEFAULT '';

ALTER TABLE export_log
    ADD CONSTRAINT export_log_status_check
        CHECK (status IN ('attempted', 'succeeded', 'failed')),
    ADD CONSTRAINT export_log_exported_bytes_check
        CHECK (exported_bytes >= 0),
    ADD CONSTRAINT export_log_export_latency_check
        CHECK (export_latency_ms >= 0),
    ADD CONSTRAINT export_log_error_code_check
        CHECK (length(error_code) <= 64);

CREATE INDEX export_log_attempted_at_idx ON export_log (attempted_at DESC);

-- +goose Down
DROP INDEX IF EXISTS export_log_attempted_at_idx;
ALTER TABLE export_log
    DROP CONSTRAINT IF EXISTS export_log_error_code_check,
    DROP CONSTRAINT IF EXISTS export_log_export_latency_check,
    DROP CONSTRAINT IF EXISTS export_log_exported_bytes_check,
    DROP CONSTRAINT IF EXISTS export_log_status_check,
    DROP COLUMN IF EXISTS error_code,
    DROP COLUMN IF EXISTS export_latency_ms,
    DROP COLUMN IF EXISTS bytes_known,
    DROP COLUMN IF EXISTS exported_bytes,
    DROP COLUMN IF EXISTS attempted_at,
    DROP COLUMN IF EXISTS status;
