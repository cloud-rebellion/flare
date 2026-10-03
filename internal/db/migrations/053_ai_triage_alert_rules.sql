-- +goose Up
-- AI triage watchdog rules page on failed calls or receipts that remain in
-- the started state past the configured age. The mode is fixed so an alert
-- rule cannot turn the watchdog into a credential, prompt, or completion
-- query. Counts remain project and organization scoped in watchdog.sql.
ALTER TABLE alert_rules
    ADD COLUMN ai_triage_mode TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_ai_triage_mode_valid
    CHECK (ai_triage_mode IN ('', 'failed', 'stuck'));

CREATE INDEX idx_ai_action_receipts_watchdog
    ON ai_action_receipts (project_id, org_id, operation, status, started_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_ai_action_receipts_watchdog;
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_ai_triage_mode_valid;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS ai_triage_mode;
