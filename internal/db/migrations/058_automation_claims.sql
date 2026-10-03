-- +goose Up
-- A workflow projection is safe to poll, but polling alone does not stop two
-- workers from running the same CI/deploy/probe concurrently.  Claims are a
-- short-lived, tenant-scoped lease for one stage and revision.  The claim id
-- is an opaque capability; it never carries source, prompt, credential, or
-- provider data.
CREATE TABLE automation_claims (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id       TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id         TEXT NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    stage            TEXT NOT NULL CHECK (stage IN ('patch', 'ci', 'merge', 'deploy', 'synthetic')),
    target_revision  TEXT NOT NULL CHECK (target_revision <> '' AND length(target_revision) <= 256),
    worker_ref       TEXT NOT NULL CHECK (worker_ref <> '' AND length(worker_ref) <= 128),
    evidence_ref     TEXT NOT NULL DEFAULT '' CHECK (length(evidence_ref) <= 256),
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'consumed', 'revoked')),
    leased_until     TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, issue_id, stage, target_revision)
);

CREATE INDEX idx_automation_claims_active
    ON automation_claims (org_id, project_id, leased_until)
    WHERE status = 'active';

ALTER TABLE automation_receipts
    ADD COLUMN claim_id TEXT REFERENCES automation_claims (id) ON DELETE SET NULL ON UPDATE SET NULL;

CREATE INDEX idx_automation_receipts_claim
    ON automation_receipts (claim_id)
    WHERE claim_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_automation_receipts_claim;
ALTER TABLE automation_receipts DROP COLUMN IF EXISTS claim_id;
DROP INDEX IF EXISTS idx_automation_claims_active;
DROP TABLE IF EXISTS automation_claims;
