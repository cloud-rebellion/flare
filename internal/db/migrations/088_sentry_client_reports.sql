-- +goose Up
-- Client reports are SDK assertions about discarded units, including deliberate
-- sampling. Store normalized counters only, under the same accepted item and
-- project boundary as the surrounding envelope. No raw payload is retained.
ALTER TABLE sentry_item_acceptances DROP CONSTRAINT sentry_item_acceptances_item_type_check;
ALTER TABLE sentry_item_acceptances ADD CONSTRAINT sentry_item_acceptances_item_type_check
    CHECK (item_type IN ('event', 'transaction', 'client_report'));

CREATE TABLE sentry_client_report_discards (
    org_id          TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    item_type       TEXT NOT NULL DEFAULT 'client_report' CHECK (item_type = 'client_report'),
    payload_sha256  TEXT NOT NULL CHECK (length(payload_sha256) = 64),
    reported_at     TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason          TEXT NOT NULL CHECK (reason IN ('sample_rate', 'span_sample_rate', 'before_send', 'event_processor', 'ignored', 'queue_overflow', 'buffer_overflow', 'cache_overflow', 'ratelimit_backoff', 'backpressure', 'network_error', 'send_error', 'internal_sdk_error', 'shutdown', 'insufficient_data', 'other')),
    category        TEXT NOT NULL CHECK (category IN ('error', 'transaction', 'span', 'log', 'log_item', 'log_byte', 'trace_metric', 'trace_metric_byte', 'monitor', 'attachment', 'profile', 'profile_chunk', 'replay', 'metric_bucket', 'metric', 'session', 'default', 'feedback', 'user_report', 'security', 'unknown', '__all__', 'other')),
    quantity        BIGINT NOT NULL CHECK (quantity BETWEEN 1 AND 1000000000),
    PRIMARY KEY (project_id, payload_sha256, reason, category),
    CONSTRAINT sentry_client_reports_project_scope_fk
        FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE,
    CONSTRAINT sentry_client_reports_acceptance_fk
        FOREIGN KEY (project_id, item_type, payload_sha256)
        REFERENCES sentry_item_acceptances (project_id, item_type, payload_sha256) ON DELETE CASCADE
);
CREATE INDEX sentry_client_reports_project_received_idx
    ON sentry_client_report_discards (project_id, org_id, received_at DESC);

-- +goose Down
DROP TABLE IF EXISTS sentry_client_report_discards;
DELETE FROM sentry_item_acceptances WHERE item_type = 'client_report';
ALTER TABLE sentry_item_acceptances DROP CONSTRAINT sentry_item_acceptances_item_type_check;
ALTER TABLE sentry_item_acceptances ADD CONSTRAINT sentry_item_acceptances_item_type_check
    CHECK (item_type IN ('event', 'transaction'));
