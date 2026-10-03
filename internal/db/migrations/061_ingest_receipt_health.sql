-- +goose Up

-- A receipt that remains in_progress after the request deadline represents an
-- acceptance uncertainty: the sender cannot know whether Flare completed the
-- request, and the completion write may have been lost during a process crash.
-- Keep the watchdog query cheap without putting payloads or credentials into
-- the health projection.
CREATE INDEX idx_ingest_receipts_in_progress_health
    ON ingest_receipts (project_id, org_id, received_at DESC)
    WHERE outcome = 'in_progress' AND completed_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_ingest_receipts_in_progress_health;
