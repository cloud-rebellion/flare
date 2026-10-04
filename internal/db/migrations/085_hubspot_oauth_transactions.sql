-- +goose Up
-- Only hashes of the browser-bound install transaction are retained. OAuth
-- credentials live encrypted in connector_connections after consent completes.
CREATE TABLE hubspot_oauth_transactions (
    state_hash BYTEA PRIMARY KEY CHECK (octet_length(state_hash) = 32),
    cookie_hash BYTEA NOT NULL CHECK (octet_length(cookie_hash) = 32),
    org_id TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    CHECK (expires_at <= started_at + interval '10 minutes')
);
CREATE INDEX hubspot_oauth_transactions_expiry ON hubspot_oauth_transactions (expires_at);

-- +goose Down
DROP TABLE IF EXISTS hubspot_oauth_transactions;
