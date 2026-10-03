-- Claims are the external worker handoff boundary.  They contain only bounded
-- opaque references and a lease; Flare never executes the claimed operation.

-- name: ClaimAutomationStage :one
WITH eligible AS (
    SELECT 1
    WHERE NOT EXISTS (
        SELECT 1
        FROM automation_receipts done
        WHERE done.org_id = sqlc.arg(org_id)
          AND done.project_id = sqlc.arg(project_id)
          AND done.issue_id = sqlc.arg(issue_id)
          AND done.stage = sqlc.arg(stage)
          AND done.status = 'passed'
          AND done.target_revision = sqlc.arg(target_revision)
    )
      AND (
          (sqlc.arg(stage) = 'patch' AND EXISTS (
              SELECT 1
              FROM ai_action_receipts ai
              WHERE ai.org_id = sqlc.arg(org_id)
                AND ai.project_id = sqlc.arg(project_id)
                AND ai.issue_id = sqlc.arg(issue_id)
                AND ai.operation = 'triage'
                AND ai.status = 'succeeded'
                AND ai.completed_at IS NOT NULL
                AND ai.evidence_digest = sqlc.arg(evidence_ref)
          ))
          OR (sqlc.arg(stage) = 'ci' AND EXISTS (
              SELECT 1
              FROM automation_receipts prior
              WHERE prior.org_id = sqlc.arg(org_id)
                AND prior.project_id = sqlc.arg(project_id)
                AND prior.issue_id = sqlc.arg(issue_id)
                AND prior.stage = 'review'
                AND prior.status = 'approved'
                AND prior.target_revision = sqlc.arg(target_revision)
                AND prior.actor_user_id IS NOT NULL
          ))
          OR (sqlc.arg(stage) = 'merge' AND EXISTS (
              SELECT 1
              FROM automation_receipts prior
              WHERE prior.org_id = sqlc.arg(org_id)
                AND prior.project_id = sqlc.arg(project_id)
                AND prior.issue_id = sqlc.arg(issue_id)
                AND prior.stage = 'ci'
                AND prior.status = 'passed'
                AND prior.target_revision = sqlc.arg(target_revision)
                AND prior.run_ref <> ''
          ))
          OR (sqlc.arg(stage) = 'deploy' AND EXISTS (
              SELECT 1
              FROM automation_receipts prior
              WHERE prior.org_id = sqlc.arg(org_id)
                AND prior.project_id = sqlc.arg(project_id)
                AND prior.issue_id = sqlc.arg(issue_id)
                AND prior.stage = 'merge'
                AND prior.status = 'passed'
                AND prior.target_revision = sqlc.arg(target_revision)
                AND prior.run_ref <> ''
          ))
          OR (sqlc.arg(stage) = 'synthetic' AND EXISTS (
              SELECT 1
              FROM automation_receipts prior
              WHERE prior.org_id = sqlc.arg(org_id)
                AND prior.project_id = sqlc.arg(project_id)
                AND prior.issue_id = sqlc.arg(issue_id)
                AND prior.stage = 'deploy'
                AND prior.status = 'passed'
                AND prior.target_revision = sqlc.arg(target_revision)
                AND prior.run_ref <> ''
          ))
      )
)
INSERT INTO automation_claims (
    id, org_id, project_id, issue_id, stage, target_revision, worker_ref,
    evidence_ref, leased_until
)
SELECT sqlc.arg(id), sqlc.arg(org_id), sqlc.arg(project_id), sqlc.arg(issue_id),
       sqlc.arg(stage), sqlc.arg(target_revision), sqlc.arg(worker_ref),
       sqlc.arg(evidence_ref), sqlc.arg(leased_until)
FROM eligible
ON CONFLICT (org_id, project_id, issue_id, stage, target_revision) DO UPDATE
SET id = EXCLUDED.id,
    worker_ref = EXCLUDED.worker_ref,
    evidence_ref = EXCLUDED.evidence_ref,
    status = 'active',
    leased_until = EXCLUDED.leased_until,
    updated_at = now()
WHERE automation_claims.status <> 'consumed'
  AND (automation_claims.status <> 'active' OR automation_claims.leased_until <= now())
RETURNING *;

-- name: GetAutomationClaim :one
SELECT *
FROM automation_claims
WHERE id = sqlc.arg(id)
  AND org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id);
