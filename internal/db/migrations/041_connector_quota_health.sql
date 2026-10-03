-- +goose Up

-- Payload-free provider quota and rate-limit hints. Values are optional because
-- providers expose different headers; they are bounded integer metadata only
-- and never include response bodies, URLs, credentials, or provider payloads.
ALTER TABLE connector_attempts
    ADD COLUMN quota_remaining BIGINT,
    ADD COLUMN retry_after_ms BIGINT;

ALTER TABLE connector_attempts
    ADD CONSTRAINT connector_attempts_quota_bounds
    CHECK ((quota_remaining IS NULL OR (quota_remaining >= 0 AND quota_remaining <= 1000000000000000))
       AND (retry_after_ms IS NULL OR (retry_after_ms >= 0 AND retry_after_ms <= 604800000)));

CREATE INDEX idx_connector_attempts_quota_latest
    ON connector_attempts (org_id, provider, operation, channel, completed_at DESC)
    WHERE quota_remaining IS NOT NULL OR retry_after_ms IS NOT NULL;

-- Connector quota rules select either the latest provider-reported remaining
-- quota or retry-after delay from the same bounded connector series.
ALTER TABLE alert_rules
    ADD COLUMN connector_quota_metric TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_connector_quota_metric_valid
    CHECK (connector_quota_metric IN ('', 'quota_remaining', 'retry_after_ms'));

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_connector_quota_metric_valid;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS connector_quota_metric;
DROP INDEX IF EXISTS idx_connector_attempts_quota_latest;
ALTER TABLE connector_attempts DROP CONSTRAINT IF EXISTS connector_attempts_quota_bounds;
ALTER TABLE connector_attempts
    DROP COLUMN IF EXISTS retry_after_ms,
    DROP COLUMN IF EXISTS quota_remaining;
