-- +goose Up

-- Release error-rate rules target one explicit release version or, when the
-- selector is empty, the latest release with a deploy marker or event in the
-- watchdog window. Keeping the selector bounded prevents a rule from turning
-- into an unbounded release query while preserving the common "latest deploy"
-- workflow.
ALTER TABLE alert_rules
    ADD COLUMN release_version TEXT NOT NULL DEFAULT '';

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_release_version_bound
    CHECK (octet_length(release_version) <= 128);

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_release_version_bound;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS release_version;
