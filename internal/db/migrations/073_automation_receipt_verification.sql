-- +goose Up
-- External execution references are caller supplied until a trusted Stage
-- outcome verifier exists. Historical rows get the same unverified status so
-- older opaque references cannot become proof merely by upgrading Flare.
ALTER TABLE automation_receipts
    ADD COLUMN verification_status TEXT NOT NULL DEFAULT 'unverified'
    CONSTRAINT automation_receipts_verification_status_check
        CHECK (verification_status IN ('unverified', 'verified'));

CREATE INDEX idx_automation_receipts_unverified_execution
    ON automation_receipts (org_id, project_id, stage, created_at DESC)
    WHERE status = 'passed'
      AND stage IN ('ci', 'merge', 'deploy', 'synthetic')
      AND verification_status = 'unverified';

-- A live lease may produce one successful advisory report. Once it expires,
-- the worker can obtain a new claim and report a fresh attempt. This also
-- prevents different idempotency keys from flooding one still-active claim.
CREATE UNIQUE INDEX uq_automation_receipts_passed_claim
    ON automation_receipts (claim_id)
    WHERE claim_id IS NOT NULL AND status = 'passed';

-- +goose Down
DROP INDEX IF EXISTS uq_automation_receipts_passed_claim;
DROP INDEX IF EXISTS idx_automation_receipts_unverified_execution;
ALTER TABLE automation_receipts DROP COLUMN IF EXISTS verification_status;
