-- +goose Up
-- Preserve the receipt and its owning organization if an old internal caller
-- assigned a project from a different organization. Only the invalid project
-- attribution is removed; provider outcome and timing remain available in
-- the organization health history. The FK from 067 already rejects new
-- mismatches while this repair runs.
UPDATE connector_attempts AS attempt
SET project_id = NULL
WHERE attempt.project_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM projects AS project
      WHERE project.id = attempt.project_id
        AND project.org_id = attempt.org_id
  );

-- Validation scans historical rows without the write-blocking lock held by
-- the earlier ADD CONSTRAINT. It follows the repair in the same transaction.
ALTER TABLE connector_attempts
    VALIDATE CONSTRAINT connector_attempts_project_org_fk;

-- +goose Down
-- Validation cannot be reversed without removing the constraint, which is
-- owned by 067's Down. The repair intentionally stays repaired.
SELECT 1;
