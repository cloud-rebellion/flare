-- +goose Up
-- Optional OTLP/HTTP retry identity for custom senders that can issue one
-- fresh key per export batch and reuse it with the same bytes on retry. The
-- ledger stores only hashes and a tenant-bound terminal receipt, never a
-- request body, header value, or telemetry payload.
CREATE TABLE otlp_acceptances (
    org_id       TEXT NOT NULL,
    project_id   TEXT NOT NULL,
    key_hash     TEXT NOT NULL CHECK (key_hash ~ '^[0-9a-f]{64}$'),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    pillar       TEXT NOT NULL CHECK (pillar IN ('logs', 'spans', 'metrics')),
    receipt_id   TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (org_id, project_id, key_hash),
    FOREIGN KEY (receipt_id, org_id, project_id)
        REFERENCES ingest_receipts (id, org_id, project_id) ON DELETE CASCADE
);
CREATE INDEX otlp_acceptances_expiry_idx ON otlp_acceptances (expires_at);

-- +goose Down
DROP TABLE IF EXISTS otlp_acceptances;
