-- +goose Up
-- The constraint swaps below take brief metadata locks on existing tables.
-- Goose wraps this migration in one transaction, so a busy table aborts the
-- whole migration without dropping the old constraint.
SET LOCAL lock_timeout = '5s';

-- Business assertions can drive operator alerts, so the public browser DSN
-- must never authorize them. A separate project-scoped key is shown once,
-- retained only by hash, and can be overlapped during rotation. Soft revocation
-- keeps the key id available as durable provenance for accepted checks.
CREATE TABLE business_check_keys (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    project_id         TEXT NOT NULL,
    name               TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64 AND name !~ '[[:cntrl:]]'),
    key_hash           TEXT NOT NULL UNIQUE CHECK (key_hash ~ '^[0-9a-f]{64}$'),
    key_prefix         TEXT NOT NULL CHECK (key_prefix ~ '^flb_[0-9a-f]{8}$'),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ NOT NULL,
    revoked_at         TIMESTAMPTZ,
    CONSTRAINT business_check_keys_project_scope_fk
        FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE,
    CONSTRAINT business_check_keys_expiry_bound
        CHECK (expires_at > created_at AND expires_at <= created_at + INTERVAL '365 days'),
    CONSTRAINT business_check_keys_id_org_project_unique UNIQUE (id, org_id, project_id)
);

CREATE INDEX idx_business_check_keys_project_created
    ON business_check_keys (org_id, project_id, created_at DESC);
CREATE INDEX idx_business_check_keys_active_by_project
    ON business_check_keys (org_id, project_id, expires_at)
    WHERE revoked_at IS NULL;

-- Existing rows satisfy the narrower prior checks. NOT VALID avoids a scan
-- of high-volume tables during deploy while enforcing the expanded vocabulary
-- on every new or updated row.
ALTER TABLE usage_buckets
    DROP CONSTRAINT usage_buckets_surface_check,
    ADD CONSTRAINT usage_buckets_surface_check
        CHECK (surface IN ('dsn', 'api_key', 'mcp', 'business_key')) NOT VALID;
ALTER TABLE alert_rules
    DROP CONSTRAINT alert_rules_usage_surface_valid,
    ADD CONSTRAINT alert_rules_usage_surface_valid
        CHECK (usage_surface IN ('', 'dsn', 'api_key', 'mcp', 'business_key')) NOT VALID;

-- This table is introduced by migration 065 in the same unreleased candidate,
-- so no older writer can have produced an unauthenticated business check.
ALTER TABLE business_checks ADD COLUMN business_key_id TEXT NOT NULL;
ALTER TABLE business_checks
    ADD CONSTRAINT business_checks_key_scope_fk
    FOREIGN KEY (business_key_id, org_id, project_id)
    REFERENCES business_check_keys (id, org_id, project_id)
    DEFERRABLE INITIALLY DEFERRED;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE business_checks DROP CONSTRAINT IF EXISTS business_checks_key_scope_fk;
ALTER TABLE business_checks DROP COLUMN IF EXISTS business_key_id;
DROP TABLE IF EXISTS business_check_keys;
-- Keep accepted usage history and alert rules intact. The old application
-- ignores business_key; the expanded checks remain safe and bounded.
