-- +goose Up
-- Payload-free, tenant-scoped receipts for the external AI coding loop. A
-- receipt records a stage transition and its bounded references only: prompts,
-- completions, source files, credentials, and untrusted payloads do not belong
-- in this table.
CREATE TABLE automation_receipts (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id       TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id         TEXT NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    stage            TEXT NOT NULL CHECK (stage IN ('triage', 'patch', 'review', 'ci', 'merge', 'deploy', 'synthetic')),
    status           TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'passed', 'failed', 'blocked')),
    idempotency_key  TEXT NOT NULL CHECK (idempotency_key <> '' AND length(idempotency_key) <= 128),
    evidence_ref     TEXT NOT NULL DEFAULT '' CHECK (length(evidence_ref) <= 256),
    source_revision  TEXT NOT NULL DEFAULT '' CHECK (length(source_revision) <= 256),
    target_revision  TEXT NOT NULL DEFAULT '' CHECK (length(target_revision) <= 256),
    run_ref          TEXT NOT NULL DEFAULT '' CHECK (length(run_ref) <= 256),
    actor_user_id    TEXT REFERENCES users (id) ON DELETE SET NULL,
    retryable        BOOLEAN NOT NULL DEFAULT false,
    error_code       TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    UNIQUE (org_id, project_id, idempotency_key),
    CHECK (status <> 'approved' OR actor_user_id IS NOT NULL),
    CHECK (status IN ('failed', 'blocked') OR error_code = ''),
    CHECK (status IN ('failed', 'blocked') OR retryable = false),
    CHECK (status IN ('passed', 'failed', 'blocked') OR completed_at IS NULL),
    CHECK (status IN ('pending', 'approved') OR completed_at IS NOT NULL)
);

CREATE INDEX idx_automation_receipts_project_created
    ON automation_receipts (org_id, project_id, created_at DESC, id DESC);
CREATE INDEX idx_automation_receipts_issue_created
    ON automation_receipts (org_id, project_id, issue_id, created_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS automation_receipts;
