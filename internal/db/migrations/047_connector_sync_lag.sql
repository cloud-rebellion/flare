-- +goose Up

-- Payload-free provider synchronization lag. A connector reports the age of
-- the source watermark it actually observed after a completed sync/read; Flare
-- never probes a provider or accepts a timestamp/URL from the caller. Keep the
-- value bounded so a public DSN cannot turn this into an arbitrary metric.
ALTER TABLE connector_attempts
    ADD COLUMN sync_lag_ms BIGINT;

ALTER TABLE connector_attempts
    ADD CONSTRAINT connector_attempts_sync_lag_bounds
    CHECK (sync_lag_ms IS NULL OR (sync_lag_ms >= 0 AND sync_lag_ms <= 604800000));

CREATE INDEX idx_connector_attempts_sync_lag_latest
    ON connector_attempts (org_id, provider, operation, channel, completed_at DESC)
    WHERE sync_lag_ms IS NOT NULL;

-- The existing connector_quota rule is a latest provider-observation rule.
-- Extend its fixed metric vocabulary for connector sync lag without creating a
-- second alert type or inferring lag from call silence.
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_connector_quota_metric_valid;
ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_connector_quota_metric_valid
    CHECK (connector_quota_metric IN ('', 'quota_remaining', 'retry_after_ms', 'sync_lag_ms'));

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_connector_quota_metric_valid;
ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_connector_quota_metric_valid
    CHECK (connector_quota_metric IN ('', 'quota_remaining', 'retry_after_ms'));
DROP INDEX IF EXISTS idx_connector_attempts_sync_lag_latest;
ALTER TABLE connector_attempts DROP CONSTRAINT IF EXISTS connector_attempts_sync_lag_bounds;
ALTER TABLE connector_attempts DROP COLUMN IF EXISTS sync_lag_ms;
