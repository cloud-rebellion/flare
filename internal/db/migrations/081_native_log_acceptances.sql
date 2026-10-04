-- +goose Up
-- A native log batch has one retry identity per authenticated project. Keep
-- only hashes and an expiry deadline; log bodies and tokens never enter this
-- ledger. The composite FK binds every acceptance to its tenant receipt.
CREATE TABLE native_log_acceptances (
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
CREATE INDEX native_log_acceptances_expiry_idx
    ON native_log_acceptances (expires_at);

-- +goose Down
DROP TABLE IF EXISTS native_log_acceptances;
