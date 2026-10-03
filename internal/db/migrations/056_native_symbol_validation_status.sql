-- +goose Up
-- Record bounded container validation alongside the retained artifact so every
-- new Flutter upload has durable, queryable evidence. Existing artifacts stay
-- not_checked until reuploaded; retaining bytes alone does not prove validity.
ALTER TABLE native_symbol_artifacts
    ADD COLUMN validation_status TEXT NOT NULL DEFAULT 'not_checked'
    CHECK (validation_status IN ('not_checked', 'recognized', 'invalid', 'unsupported'));

-- +goose Down
ALTER TABLE native_symbol_artifacts DROP COLUMN validation_status;
