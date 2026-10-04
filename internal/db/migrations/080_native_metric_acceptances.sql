-- +goose Up
-- A native metric batch has one retry identity per authenticated project. Keep
-- only hashes, counts in its terminal ingest receipt, and an expiry deadline;
-- metric labels and request bodies never enter this ledger. The composite FK
-- prevents a cross-project receipt from backing an acceptance.
CREATE TABLE native_metric_acceptances (
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
CREATE INDEX native_metric_acceptances_expiry_idx
    ON native_metric_acceptances (expires_at);

-- +goose Down
DROP TABLE IF EXISTS native_metric_acceptances;
