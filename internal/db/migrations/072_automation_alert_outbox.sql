-- +goose Up
SET LOCAL lock_timeout = '5s';

-- A durable, payload-free notice for one automation channel. The delivery ID
-- and observed_at never change across attempts, allowing receivers to dedupe
-- retries even though each attempt gets a fresh replay-safe HMAC timestamp.
-- No issue title, stack trace, channel config, or credential is copied here.
CREATE TABLE automation_alert_outbox (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL,
    project_id        TEXT NOT NULL,
    channel_id        TEXT,
    issue_id          TEXT NOT NULL DEFAULT '' CHECK (length(issue_id) <= 128),
    event             TEXT NOT NULL CHECK (event IN (
        'new_issue', 'regression', 'spike', 'anomaly', 'silence',
        'monitor_missing', 'deploy_failure', 'deploy_acceptance', 'rollback',
        'connector', 'release', 'service', 'log', 'usage', 'ai', 'alert', 'test'
    )),
    event_count       BIGINT NOT NULL DEFAULT 0 CHECK (event_count BETWEEN 0 AND 1000000000),
    observed_at       TIMESTAMPTZ NOT NULL,
    state             TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'leased', 'delivered', 'dead', 'cancelled')),
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 12),
    next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token       TEXT NOT NULL DEFAULT '',
    leased_until      TIMESTAMPTZ,
    last_error_code   TEXT NOT NULL DEFAULT '' CHECK (last_error_code IN ('', 'delivery_failed', 'route_removed', 'channel_removed')),
    delivered_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT automation_alert_outbox_project_scope_fk
        FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE,
    CONSTRAINT automation_alert_outbox_channel_scope_fk
        FOREIGN KEY (channel_id, org_id) REFERENCES notification_channels (id, org_id) ON DELETE SET NULL (channel_id),
    CONSTRAINT automation_alert_outbox_lease_shape CHECK (
        (state = 'leased' AND lease_token <> '' AND leased_until IS NOT NULL)
        OR (state <> 'leased' AND lease_token = '' AND leased_until IS NULL)
    )
);

CREATE INDEX automation_alert_outbox_due_idx
    ON automation_alert_outbox (next_attempt_at, id)
    WHERE state = 'pending';
CREATE INDEX automation_alert_outbox_expired_idx
    ON automation_alert_outbox (leased_until, id)
    WHERE state = 'leased';
CREATE INDEX automation_alert_outbox_dead_idx
    ON automation_alert_outbox (created_at)
    WHERE state = 'dead';
CREATE INDEX automation_alert_outbox_project_idx
    ON automation_alert_outbox (org_id, project_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS automation_alert_outbox;
