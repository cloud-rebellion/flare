-- +goose Up
-- One optional retry identity per DSN-authenticated project. Store only
-- hashes and an expiry deadline, never provider payloads, raw JSON, or keys.
-- The composite FK forbids a cross-tenant or cross-project receipt.
CREATE TABLE client_connector_acceptances (
    org_id       TEXT NOT NULL,
    project_id   TEXT NOT NULL,
    key_hash     TEXT NOT NULL CHECK (key_hash ~ '^[0-9a-f]{64}$'),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    receipt_id   TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (org_id, project_id, key_hash),
    FOREIGN KEY (receipt_id, org_id, project_id)
        REFERENCES ingest_receipts (id, org_id, project_id) ON DELETE CASCADE
);
CREATE INDEX client_connector_acceptances_expiry_idx
    ON client_connector_acceptances (expires_at);

-- +goose Down
DROP TABLE IF EXISTS client_connector_acceptances;
