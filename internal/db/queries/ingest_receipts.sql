-- Payload-free, tenant-scoped ingest receipts. These rows contain counts and
-- status only; raw request bodies and credentials must never be persisted here.

-- name: InsertIngestReceipt :one
INSERT INTO ingest_receipts (
    id, request_id, org_id, project_id, pillar, protocol,
    received_count, accepted_count, rejected_count, dropped_count,
    persisted_count, outcome, http_status, retryable, received_at,
    completed_at
)
VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10,
    $11, $12, $13, $14, $15,
    $16
)
RETURNING *;

-- name: CompleteIngestReceipt :one
UPDATE ingest_receipts
SET received_count = $4,
    accepted_count = $5,
    rejected_count = $6,
    dropped_count = $7,
    persisted_count = $8,
    outcome = $9,
    http_status = $10,
    retryable = $11,
    completed_at = $12
WHERE id = $1 AND org_id = $2 AND project_id = $3
RETURNING *;

-- name: GetIngestReceipt :one
SELECT *
FROM ingest_receipts
WHERE id = $1 AND org_id = $2 AND project_id = $3;

-- name: ListIngestReceiptsByProject :many
SELECT *
FROM ingest_receipts
WHERE org_id = $1 AND project_id = $2
ORDER BY received_at DESC, id DESC
LIMIT $3;
