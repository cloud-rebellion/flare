-- +goose Up
-- Payload-free, org-scoped health receipts for outbound connectors. The
-- optional project_id ties a connector attempt to a project when one exists;
-- org-level connectors (AI, OIDC, and channel tests) leave it NULL.
CREATE TABLE connector_attempts (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id   TEXT REFERENCES projects (id) ON DELETE SET NULL,
    provider     TEXT NOT NULL CHECK (provider <> '' AND length(provider) <= 32),
    operation    TEXT NOT NULL CHECK (operation <> '' AND length(operation) <= 64),
    channel      TEXT NOT NULL CHECK (channel <> '' AND length(channel) <= 32),
    outcome      TEXT NOT NULL CHECK (outcome IN ('ok', 'error')),
    status_class TEXT NOT NULL DEFAULT '' CHECK (length(status_class) <= 16),
    error_code   TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    elapsed_ms   BIGINT NOT NULL CHECK (elapsed_ms >= 0),
    attempted_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL,
    CHECK (outcome = 'ok' OR status_class <> '' OR error_code <> '')
);

CREATE INDEX idx_connector_attempts_org_completed
    ON connector_attempts (org_id, completed_at DESC);
CREATE INDEX idx_connector_attempts_project_completed
    ON connector_attempts (org_id, project_id, completed_at DESC);

-- +goose Down
DROP TABLE IF EXISTS connector_attempts;
