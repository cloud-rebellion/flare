-- +goose Up
-- Refuse a busy live-schema lock rather than queueing behind serving traffic.
SET LOCAL lock_timeout = '5s';
-- Cookie proof stays browser-bound. Only one-use hashes, tenant/configuration
-- binding and expiry are retained; no PKCE verifier, token or personal data.
CREATE TABLE oidc_login_transactions (
    state_hash BYTEA PRIMARY KEY CHECK (octet_length(state_hash) = 32),
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    config_hash BYTEA NOT NULL CHECK (octet_length(config_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at <= created_at + interval '10 minutes')
);
CREATE INDEX oidc_login_transactions_org_expiry ON oidc_login_transactions(org_id, expires_at);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE IF EXISTS oidc_login_transactions;
