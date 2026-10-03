-- +goose Up
-- A deterministic fingerprint of the bounded, scrubbed incident context sent
-- to the AI provider. Keep only the SHA-256 digest: prompts, completions,
-- credentials, and event payloads do not belong in a receipt.
ALTER TABLE ai_action_receipts
    ADD COLUMN evidence_digest TEXT NOT NULL DEFAULT ''
    CHECK (evidence_digest = '' OR evidence_digest ~ '^[0-9a-f]{64}$');

-- +goose Down
ALTER TABLE ai_action_receipts
    DROP COLUMN IF EXISTS evidence_digest;
