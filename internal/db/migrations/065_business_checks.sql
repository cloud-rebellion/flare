-- +goose Up
-- One bounded, payload-free result of a business assertion, such as a
-- checkout, payment, or account provisioning outcome. The server derives
-- tenant scope and timestamps from the confidential project business key. A result
-- and its accepted ingest receipt are committed in one transaction.
CREATE TABLE business_checks (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    receipt_id   TEXT NOT NULL UNIQUE,
    -- Only hashes are retained. The scoped unique key turns an ambiguous
    -- response into a safe retry without exposing a caller's opaque key.
    idempotency_key_hash TEXT NOT NULL CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    check_name   TEXT NOT NULL CHECK (check_name ~ '^[a-z][a-z0-9._:-]{0,63}$'),
    outcome      TEXT NOT NULL CHECK (outcome IN ('ok', 'error')),
    status_class TEXT NOT NULL DEFAULT '' CHECK (status_class = '' OR status_class ~ '^[1-5]xx$'),
    error_code   TEXT NOT NULL DEFAULT '' CHECK (error_code = '' OR error_code ~ '^[a-z0-9][a-z0-9._:-]{0,63}$'),
    elapsed_ms   BIGINT NOT NULL CHECK (elapsed_ms BETWEEN 0 AND 86400000),
    completed_at TIMESTAMPTZ NOT NULL,
    CHECK (outcome = 'error' OR (status_class IN ('', '2xx') AND error_code = '')),
    CHECK (outcome <> 'error' OR status_class <> '2xx'),
    CHECK (outcome = 'ok' OR status_class <> '' OR error_code <> ''),
    CONSTRAINT business_checks_receipt_scope_fk
        FOREIGN KEY (receipt_id, org_id, project_id)
        REFERENCES ingest_receipts (id, org_id, project_id) ON DELETE CASCADE,
    CONSTRAINT business_checks_idempotency_scope_unique
        UNIQUE (org_id, project_id, idempotency_key_hash)
);

CREATE INDEX idx_business_checks_project_name_completed
    ON business_checks (project_id, org_id, check_name, completed_at DESC);
CREATE INDEX idx_business_checks_project_completed_name
    ON business_checks (project_id, org_id, completed_at DESC, check_name);

-- +goose Down
DROP TABLE IF EXISTS business_checks;
