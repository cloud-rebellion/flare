-- +goose Up
SET LOCAL lock_timeout = '5s';

-- An error check-in starts one durable failure episode. Its generation stays
-- unchanged on repeated error pings and advances only after a recovery, so
-- flapping cannot enqueue one notice per request.
ALTER TABLE monitors
    ADD COLUMN failed_generation BIGINT NOT NULL DEFAULT 0
        CHECK (failed_generation >= 0);
UPDATE monitors SET failed_generation=1 WHERE state='failed';
ALTER TABLE monitors
    ADD CONSTRAINT monitors_failure_outbox_scope UNIQUE (id, org_id, project_id);

-- One reusable row per monitor and channel bounds storage by the existing
-- monitor/channel caps. A new failure episode resets this row; successful
-- delivery and cancellation stay visible without retaining unbounded history.
-- No message body, destination, channel config, or credential is stored here.
CREATE TABLE monitor_failure_outbox (
    monitor_id         TEXT NOT NULL,
    org_id             TEXT NOT NULL,
    project_id         TEXT NOT NULL,
    channel_id         TEXT NOT NULL,
    failed_generation  BIGINT NOT NULL CHECK (failed_generation > 0),
    observed_at        TIMESTAMPTZ NOT NULL,
    state              TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'leased', 'delivered', 'cancelled')),
    attempts           INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 1000000000),
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token        TEXT NOT NULL DEFAULT '',
    leased_until       TIMESTAMPTZ,
    last_error_code    TEXT NOT NULL DEFAULT ''
        CHECK (last_error_code IN ('', 'delivery_failed', 'route_removed')),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (monitor_id, channel_id),
    FOREIGN KEY (monitor_id, org_id, project_id)
        REFERENCES monitors (id, org_id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (channel_id, org_id)
        REFERENCES notification_channels (id, org_id) ON DELETE CASCADE,
    CONSTRAINT monitor_failure_outbox_lease_shape CHECK (
        (state='leased' AND lease_token<>'' AND leased_until IS NOT NULL)
        OR (state<>'leased' AND lease_token='' AND leased_until IS NULL)
    )
);
CREATE INDEX monitor_failure_outbox_due_idx
    ON monitor_failure_outbox (next_attempt_at, monitor_id, channel_id)
    WHERE state='pending';
CREATE INDEX monitor_failure_outbox_expired_idx
    ON monitor_failure_outbox (leased_until, monitor_id, channel_id)
    WHERE state='leased';

-- +goose Down
DROP TABLE IF EXISTS monitor_failure_outbox;
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_failure_outbox_scope;
ALTER TABLE monitors DROP COLUMN IF EXISTS failed_generation;
