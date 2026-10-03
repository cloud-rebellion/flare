-- +goose Up
-- Payload-free, tenant-scoped usage accounting for authenticated boundaries.
--
-- A bucket records aggregate request/error deltas rather than credentials,
-- request paths, bodies, IP addresses, or user identifiers.  DSN project IDs
-- and API-key IDs are bounded opaque identifiers; they are retained only so an
-- authenticated tenant can distinguish its own projects and keys.  An empty
-- project_id/credential_id means the signal is org-wide (MCP sessions and
-- API-key requests that could not be attributed to a project).
CREATE TABLE usage_buckets (
    org_id          TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id      TEXT NOT NULL DEFAULT '' CHECK (length(project_id) <= 128),
    credential_id   TEXT NOT NULL DEFAULT '' CHECK (length(credential_id) <= 128),
    surface         TEXT NOT NULL CHECK (surface IN ('dsn', 'api_key', 'mcp')),
    operation       TEXT NOT NULL CHECK (operation <> '' AND length(operation) <= 64),
    bucket_start    TIMESTAMPTZ NOT NULL,
    request_count   BIGINT NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    error_count     BIGINT NOT NULL DEFAULT 0 CHECK (error_count >= 0),
    denied_count    BIGINT NOT NULL DEFAULT 0 CHECK (denied_count >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, project_id, credential_id, surface, operation, bucket_start)
);

CREATE INDEX idx_usage_buckets_org_started
    ON usage_buckets (org_id, bucket_start DESC);

-- +goose Down
DROP TABLE IF EXISTS usage_buckets;
