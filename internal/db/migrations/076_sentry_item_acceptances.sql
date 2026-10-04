-- +goose Up

-- One durable, payload-free identity per accepted Sentry event/transaction.
-- The row is inserted in the same transaction as the telemetry and alert job.
-- The payload hash catches exact replays even when an SDK omits event_id; the
-- optional item ID rejects a reused identity with different bytes.
CREATE TABLE sentry_item_acceptances (
    org_id          TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    item_type       TEXT NOT NULL CHECK (item_type IN ('event', 'transaction')),
    item_id         TEXT CHECK (item_id IS NULL OR (item_id <> '' AND length(item_id) <= 128)),
    payload_sha256  TEXT NOT NULL CHECK (length(payload_sha256) = 64),
    accepted_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, item_type, payload_sha256),
    CONSTRAINT sentry_item_acceptances_project_scope_fk
        FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX sentry_item_acceptances_id_key
    ON sentry_item_acceptances (project_id, item_type, item_id)
    WHERE item_id IS NOT NULL;
CREATE INDEX sentry_item_acceptances_accepted_at_idx
    ON sentry_item_acceptances (accepted_at);

-- +goose Down
DROP TABLE IF EXISTS sentry_item_acceptances;
