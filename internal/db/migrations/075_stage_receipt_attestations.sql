-- +goose Up
-- Stage access is bound to one Flare project. The credential never appears in
-- public projections and every update rotates the binding epoch, fencing an
-- in-flight verification against a concurrent reconfiguration.
CREATE TABLE automation_stage_bindings (
    project_id TEXT PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    org_id TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    stage_tenant_id TEXT NOT NULL CHECK (stage_tenant_id <> '' AND length(stage_tenant_id) <= 128),
    stage_credential_id TEXT NOT NULL CHECK (stage_credential_id <> '' AND length(stage_credential_id) <= 256),
    repository TEXT NOT NULL CHECK (repository <> '' AND length(repository) <= 256),
    program_id TEXT NOT NULL CHECK (program_id <> '' AND length(program_id) <= 128),
    ci_pipeline TEXT NOT NULL CHECK (ci_pipeline <> '' AND length(ci_pipeline) <= 128),
    merge_pipeline TEXT NOT NULL DEFAULT 'controlled-merge' CHECK (merge_pipeline = 'controlled-merge'),
    deploy_pipeline TEXT NOT NULL CHECK (deploy_pipeline <> '' AND length(deploy_pipeline) <= 128),
    credential_ciphertext TEXT NOT NULL CHECK (credential_ciphertext <> ''),
    binding_epoch TEXT NOT NULL CHECK (binding_epoch <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id)
);

-- The proof row is an immutable, payload-free record of the exact assertion
-- accepted from Stage. It is inserted in one transaction with the receipt
-- promotion and the ordinary audit log entry. No client can write this table.
CREATE TABLE automation_receipt_attestations (
    receipt_id TEXT PRIMARY KEY REFERENCES automation_receipts (id) ON DELETE CASCADE,
    org_id TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id TEXT NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    stage_tenant_id TEXT NOT NULL,
    stage_credential_id TEXT NOT NULL,
    attestation_id TEXT NOT NULL CHECK (attestation_id <> '' AND length(attestation_id) <= 256),
    repository TEXT NOT NULL,
    program_id TEXT NOT NULL,
    pipeline TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('ci', 'merge', 'deploy', 'synthetic')),
    run_ref TEXT NOT NULL,
    evidence_ref TEXT NOT NULL,
    source_revision TEXT NOT NULL,
    target_revision TEXT NOT NULL,
    image_ref TEXT NOT NULL DEFAULT '',
    image_id TEXT NOT NULL DEFAULT '',
    stage_verified_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, receipt_id)
);

CREATE INDEX idx_automation_receipt_attestations_project
    ON automation_receipt_attestations (org_id, project_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS automation_receipt_attestations;
DROP TABLE IF EXISTS automation_stage_bindings;
