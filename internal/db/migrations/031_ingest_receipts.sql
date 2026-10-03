-- +goose Up
-- Payload-free durable ingest accounting. A receipt records what Flare saw and
-- what it durably handled without retaining request bodies or tenant secrets.
-- Both tenant keys are retained on every row so readers can enforce the same
-- project and org boundary as the telemetry tables.
CREATE TABLE ingest_receipts (
    id              TEXT PRIMARY KEY,
    request_id      TEXT,
    org_id          TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id      TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    pillar          TEXT NOT NULL CHECK (pillar <> '' AND length(pillar) <= 32),
    protocol        TEXT NOT NULL CHECK (protocol <> '' AND length(protocol) <= 32),
    received_count  BIGINT NOT NULL DEFAULT 0 CHECK (received_count >= 0),
    accepted_count  BIGINT NOT NULL DEFAULT 0 CHECK (accepted_count >= 0),
    rejected_count  BIGINT NOT NULL DEFAULT 0 CHECK (rejected_count >= 0),
    dropped_count   BIGINT NOT NULL DEFAULT 0 CHECK (dropped_count >= 0),
    persisted_count BIGINT NOT NULL DEFAULT 0 CHECK (persisted_count >= 0),
    outcome         TEXT NOT NULL CHECK (outcome <> '' AND length(outcome) <= 32),
    http_status     SMALLINT NOT NULL CHECK (http_status BETWEEN 100 AND 599),
    retryable       BOOLEAN NOT NULL DEFAULT false,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (request_id IS NULL OR length(request_id) <= 255)
);

CREATE INDEX idx_ingest_receipts_project_received
    ON ingest_receipts (project_id, org_id, received_at DESC);
CREATE INDEX idx_ingest_receipts_org_received
    ON ingest_receipts (org_id, received_at DESC);
CREATE INDEX idx_ingest_receipts_request
    ON ingest_receipts (org_id, project_id, request_id)
    WHERE request_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS ingest_receipts;
