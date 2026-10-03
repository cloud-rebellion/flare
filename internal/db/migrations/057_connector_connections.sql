-- +goose Up
-- Tenant-owned connector connection metadata. The secret column contains the
-- output of internal/secretbox and is never selected by the public projections.
-- Provider-specific polling and scheduling deliberately live outside this
-- foundation; this table is the durable connection registry they will use.
CREATE TABLE connector_connections (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    provider            TEXT NOT NULL CHECK (char_length(provider) BETWEEN 1 AND 32),
    installation_id     TEXT NOT NULL CHECK (char_length(installation_id) BETWEEN 1 AND 128),
    installation_name   TEXT NOT NULL DEFAULT '' CHECK (char_length(installation_name) <= 128),
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'active', 'paused', 'error', 'revoked')),
    scopes              JSONB NOT NULL DEFAULT '[]'::jsonb,
    secret_ciphertext   TEXT NOT NULL DEFAULT '' CHECK (char_length(secret_ciphertext) <= 20000),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, provider, installation_id),
    CHECK (jsonb_typeof(scopes) = 'array')
);
CREATE INDEX idx_connector_connections_org_updated
    ON connector_connections (org_id, updated_at DESC);

-- +goose Down
DROP TABLE IF EXISTS connector_connections;
