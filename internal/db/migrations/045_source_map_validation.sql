-- +goose Up
-- Bounded source-map parse outcome. Uploads remain durable for compatibility;
-- this fixed status tells callers whether Flare's read-time resolver can use
-- the retained document without exposing map bytes or parser messages.
ALTER TABLE source_map_artifacts
    ADD COLUMN parse_status TEXT NOT NULL DEFAULT 'unvalidated';

ALTER TABLE source_map_artifacts
    ADD CONSTRAINT source_map_artifacts_parse_status_valid
    CHECK (parse_status IN ('unvalidated', 'valid', 'invalid_json', 'invalid_mappings', 'unsupported'));

-- +goose Down
ALTER TABLE source_map_artifacts
    DROP CONSTRAINT IF EXISTS source_map_artifacts_parse_status_valid;
ALTER TABLE source_map_artifacts
    DROP COLUMN IF EXISTS parse_status;
