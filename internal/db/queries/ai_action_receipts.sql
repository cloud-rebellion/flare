-- Payload-free, tenant-scoped AI action receipts. Prompts, responses,
-- credentials, and provider error text must never be persisted here. The
-- evidence_digest is only a fixed SHA-256 fingerprint of the bounded,
-- scrubbed context used for a fresh provider call.

-- name: InsertAIActionReceipt :one
INSERT INTO ai_action_receipts (
    id, org_id, project_id, issue_id, operation, status, provider,
    started_at, retryable, error_code, evidence_digest
)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
WHERE EXISTS (
    SELECT 1 FROM issues
    WHERE id = $4 AND org_id = $2 AND project_id = $3
)
RETURNING *;

-- name: CompleteAIActionReceipt :one
UPDATE ai_action_receipts
SET status = $4,
    provider = $5,
    completed_at = $6,
    retryable = $7,
    error_code = $8,
    evidence_digest = $9
WHERE id = $1 AND org_id = $2 AND project_id = $3
RETURNING *;

-- name: ListAIActionReceiptsByProject :many
SELECT *
FROM ai_action_receipts
WHERE org_id = $1 AND project_id = $2
ORDER BY started_at DESC, id DESC
LIMIT $3;
