-- Payload-free receipts for the external AI coding loop. Keep references
-- bounded and never add prompts, completions, credentials, source contents, or
-- provider response text.

-- name: InsertAutomationReceipt :one
-- The claim CTE is an atomic handoff. A passed patch consumes its lease after
-- the API checks its AI triage digest. External passed receipts remain
-- unverified advisory rows until Stage evidence is checked by a trusted
-- verifier, so they do not consume the stage or block a later retry.
-- Human review and rollback receipts intentionally have no claim.
WITH claim AS (
    SELECT id
    FROM automation_claims ac
    WHERE COALESCE(sqlc.arg(claim_id)::text, '') <> ''
      AND ac.id = sqlc.arg(claim_id)
      AND ac.org_id = sqlc.arg(org_id)
      AND ac.project_id = sqlc.arg(project_id)
      AND ac.issue_id = sqlc.arg(issue_id)
      AND ac.stage = sqlc.arg(stage)
      AND ac.target_revision = sqlc.arg(target_revision)
      -- Only patch claims carry AI evidence. CI, merge, deploy, and
      -- synthetic claims bind the stage/revision/lease; their receipt may
      -- carry its own bounded run or probe evidence reference.
      AND (ac.stage <> 'patch' OR ac.evidence_ref = sqlc.arg(evidence_ref))
      AND ac.status = 'active'
      AND ac.leased_until > now()
    FOR UPDATE
), inserted AS (
    INSERT INTO automation_receipts (
        id, org_id, project_id, issue_id, stage, status, idempotency_key,
        evidence_ref, source_revision, target_revision, run_ref, actor_user_id,
        retryable, error_code, completed_at, claim_id
    )
    SELECT sqlc.arg(id), sqlc.arg(org_id), sqlc.arg(project_id), sqlc.arg(issue_id),
           sqlc.arg(stage), sqlc.arg(status), sqlc.arg(idempotency_key),
           sqlc.arg(evidence_ref), sqlc.arg(source_revision), sqlc.arg(target_revision),
           sqlc.arg(run_ref), sqlc.arg(actor_user_id), sqlc.arg(retryable),
           sqlc.arg(error_code), sqlc.arg(completed_at), NULLIF(COALESCE(sqlc.arg(claim_id)::text, ''), '')
    FROM issues i
    LEFT JOIN claim c ON true
    WHERE i.id = sqlc.arg(issue_id)
      AND i.org_id = sqlc.arg(org_id)
      AND i.project_id = sqlc.arg(project_id)
      AND (
          (COALESCE(sqlc.arg(claim_id)::text, '') = '' AND (
              sqlc.arg(status) <> 'passed'
              OR sqlc.arg(stage) NOT IN ('patch', 'ci', 'merge', 'deploy', 'synthetic')
          ))
          OR c.id IS NOT NULL
      )
    ON CONFLICT DO NOTHING
    RETURNING *
), consumed AS (
    UPDATE automation_claims ac
    SET status = CASE WHEN sqlc.arg(status) = 'passed' AND sqlc.arg(stage) = 'patch' THEN 'consumed' ELSE ac.status END,
        updated_at = now()
    FROM inserted i
    WHERE ac.id = i.claim_id
    RETURNING i.*
)
SELECT i.*
FROM inserted i
LEFT JOIN consumed c ON c.id = i.id;

-- name: GetAutomationReceiptByIdempotency :one
SELECT *
FROM automation_receipts
WHERE org_id = $1 AND project_id = $2 AND idempotency_key = $3;

-- name: ListAutomationReceiptsByProject :many
SELECT *
FROM automation_receipts
WHERE org_id = $1 AND project_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListAutomationReceiptsByIssue :many
-- The automation worker polls one issue at a time. Keep this projection
-- tenant/project scoped and bounded; the API computes the next eligible stage
-- without exposing prompts, source contents, or provider payloads.
SELECT *
FROM (
    SELECT *
    FROM automation_receipts
    WHERE org_id = $1 AND project_id = $2 AND issue_id = $3
    ORDER BY created_at DESC, id DESC
    LIMIT $4
) AS recent
ORDER BY created_at ASC, id ASC;

-- name: GetAutomationVerificationStats :one
-- Project-scoped, bounded recent backlog for external execution results that
-- have no Stage-owned proof yet. These rows remain visible even though they
-- cannot advance the workflow or count as accepted deployment evidence.
SELECT count(*)::bigint AS pending_count,
       count(*) FILTER (WHERE stage = 'ci')::bigint AS ci_count,
       count(*) FILTER (WHERE stage = 'merge')::bigint AS merge_count,
       count(*) FILTER (WHERE stage = 'deploy')::bigint AS deploy_count,
       count(*) FILTER (WHERE stage = 'synthetic')::bigint AS synthetic_count
FROM automation_receipts
WHERE org_id = sqlc.arg(org_id)::text
  AND project_id = sqlc.arg(project_id)::text
  AND status = 'passed'
  AND stage IN ('ci', 'merge', 'deploy', 'synthetic')
  AND verification_status = 'unverified'
  AND created_at >= sqlc.arg(since);

-- name: HasAutomationReceiptEvidence :one
-- A completed coding-loop stage is only evidence when it follows the human
-- approved revision that it claims to advance. Keep all tenant, project, issue,
-- stage, status, and revision predicates in this query; a revision string is
-- caller supplied and cannot identify a workflow by itself.
SELECT EXISTS (
    SELECT 1
    FROM automation_receipts ar
    WHERE ar.org_id = sqlc.arg(org_id)
      AND ar.project_id = sqlc.arg(project_id)
      AND ar.issue_id = sqlc.arg(issue_id)
      AND ar.stage = sqlc.arg(stage)
      AND ar.status = sqlc.arg(status)
      AND ar.target_revision = sqlc.arg(target_revision)
      AND (
          -- A review approval remains human evidence only while its actor
          -- identity is retained; the table check rejects actor deletion
          -- while an approved receipt still references that user.
          (ar.stage = 'review' AND ar.actor_user_id IS NOT NULL)
          -- Opaque caller references are not outcome proof. Only a trusted
          -- server-side verifier may promote an external execution receipt.
          OR (ar.stage IN ('ci', 'merge', 'deploy', 'synthetic')
              AND ar.run_ref <> '' AND ar.verification_status = 'verified'
              AND (ar.stage <> 'synthetic' OR ar.evidence_ref <> ''))
          -- Patch completion must point to the exact completed AI evidence
          -- that the worker used; a revision marker alone is insufficient.
          OR (
              ar.stage = 'patch'
              AND EXISTS (
                  SELECT 1
                  FROM ai_action_receipts ai
                  WHERE ai.org_id = ar.org_id
                    AND ai.project_id = ar.project_id
                    AND ai.issue_id = ar.issue_id
                    AND ai.operation = 'triage'
                    AND ai.status = 'succeeded'
                    AND ai.completed_at IS NOT NULL
                    AND ai.evidence_digest = ar.evidence_ref
              )
          )
      )
) AS exists;

-- name: HasSuccessfulAIActionEvidence :one
-- A patch may advance only the exact, completed fresh-triage evidence that
-- the worker reports. Keep the digest and every tenant/project/issue
-- predicate together; the digest is opaque outside this query and must never
-- become a cross-tenant lookup key.
SELECT EXISTS (
    SELECT 1
    FROM ai_action_receipts
    WHERE org_id = sqlc.arg(org_id)
      AND project_id = sqlc.arg(project_id)
      AND issue_id = sqlc.arg(issue_id)
      AND operation = 'triage'
      AND status = 'succeeded'
      AND completed_at IS NOT NULL
      AND evidence_digest = sqlc.arg(evidence_digest)
) AS exists;
