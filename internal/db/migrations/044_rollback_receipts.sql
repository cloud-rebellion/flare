-- +goose Up
-- Rollback is an explicit workflow stage. It must never be inferred from an
-- ordinary deploy receipt: callers record the rollback target and source
-- revision in the payload-free receipt itself.
ALTER TABLE automation_receipts
    DROP CONSTRAINT IF EXISTS automation_receipts_stage_check;
ALTER TABLE automation_receipts
    ADD CONSTRAINT automation_receipts_stage_check
    CHECK (stage IN ('triage', 'patch', 'review', 'ci', 'merge', 'deploy', 'rollback', 'synthetic'));

-- +goose Down
ALTER TABLE automation_receipts
    DROP CONSTRAINT IF EXISTS automation_receipts_stage_check;
ALTER TABLE automation_receipts
    ADD CONSTRAINT automation_receipts_stage_check
    CHECK (stage IN ('triage', 'patch', 'review', 'ci', 'merge', 'deploy', 'synthetic'));
