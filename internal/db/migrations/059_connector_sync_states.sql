-- +goose Up
-- Provider-agnostic synchronization state for a registered connector. This is
-- control-plane metadata only: cursors are encrypted at the application
-- boundary, provider payloads and response text never enter this table.
--
-- Keep the organization on both rows and enforce the relationship in Postgres
-- so a connection from one tenant cannot be paired with sync state from
-- another tenant even if a future worker makes a query mistake.
ALTER TABLE connector_connections
    ADD CONSTRAINT connector_connections_id_org_unique UNIQUE (id, org_id);

CREATE TABLE connector_sync_states (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    connection_id       TEXT NOT NULL,
    operation           TEXT NOT NULL CHECK (char_length(operation) BETWEEN 1 AND 64),
    state               TEXT NOT NULL DEFAULT 'idle'
                        CHECK (state IN ('idle', 'running', 'succeeded', 'failed')),
    cursor_ciphertext   TEXT NOT NULL DEFAULT '' CHECK (char_length(cursor_ciphertext) <= 20000),
    cursor_version      BIGINT NOT NULL DEFAULT 0 CHECK (cursor_version >= 0),
    attempt_count       BIGINT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_started_at     TIMESTAMPTZ,
    last_completed_at   TIMESTAMPTZ,
    last_success_at     TIMESTAMPTZ,
    last_error_code     TEXT NOT NULL DEFAULT '' CHECK (char_length(last_error_code) <= 64),
    last_sync_lag_ms    BIGINT CHECK (last_sync_lag_ms IS NULL OR (last_sync_lag_ms >= 0 AND last_sync_lag_ms <= 604800000)),
    next_run_at         TIMESTAMPTZ,
    lease_owner         TEXT NOT NULL DEFAULT '' CHECK (char_length(lease_owner) <= 128),
    lease_expires_at    TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (connection_id, org_id)
        REFERENCES connector_connections (id, org_id) ON DELETE CASCADE,
    UNIQUE (org_id, connection_id, operation),
    CHECK ((lease_owner = '' AND lease_expires_at IS NULL)
        OR (lease_owner <> '' AND lease_expires_at IS NOT NULL))
);

CREATE INDEX idx_connector_sync_states_org_updated
    ON connector_sync_states (org_id, updated_at DESC, id DESC);
CREATE INDEX idx_connector_sync_states_due
    ON connector_sync_states (org_id, next_run_at ASC, updated_at DESC)
    WHERE state <> 'running';

-- +goose Down
DROP INDEX IF EXISTS idx_connector_sync_states_due;
DROP INDEX IF EXISTS idx_connector_sync_states_org_updated;
DROP TABLE IF EXISTS connector_sync_states;
ALTER TABLE connector_connections
    DROP CONSTRAINT IF EXISTS connector_connections_id_org_unique;
