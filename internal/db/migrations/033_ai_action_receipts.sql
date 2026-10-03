-- +goose Up
-- Payload-free durable receipts for AI triage and future bounded AI actions.
-- These rows prove whether a tenant-scoped action ran without storing prompts,
-- completions, provider credentials, or untrusted error text.
CREATE TABLE ai_action_receipts (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id     TEXT NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    operation    TEXT NOT NULL CHECK (operation <> '' AND length(operation) <= 32),
    status       TEXT NOT NULL CHECK (status IN ('started', 'succeeded', 'cached', 'failed')),
    provider     TEXT NOT NULL CHECK (provider <> '' AND length(provider) <= 32),
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    retryable    BOOLEAN NOT NULL DEFAULT false,
    error_code   TEXT CHECK (error_code IS NULL OR (error_code <> '' AND length(error_code) <= 64)),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ai_action_receipts_project_started
    ON ai_action_receipts (project_id, org_id, started_at DESC, id DESC);
CREATE INDEX idx_ai_action_receipts_issue_started
    ON ai_action_receipts (issue_id, org_id, started_at DESC, id DESC);
CREATE INDEX idx_ai_action_receipts_org_started
    ON ai_action_receipts (org_id, started_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS ai_action_receipts;
