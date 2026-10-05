-- +goose Up
-- A retired bundle's immutable metadata survives deletion of its map bytes.
-- The row and audit entry are inserted in the same transaction as erasure.
CREATE TABLE source_map_bundle_retirements (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    bundle_id TEXT NOT NULL,
    release TEXT NOT NULL,
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    artifact_count INTEGER NOT NULL CHECK (artifact_count > 0),
    total_bytes BIGINT NOT NULL CHECK (total_bytes > 0),
    actor_user_id TEXT NOT NULL,
    protection_ref TEXT NOT NULL CHECK (protection_ref <> '' AND length(protection_ref) <= 256),
    protection_generation TEXT NOT NULL CHECK (protection_generation <> '' AND length(protection_generation) <= 128),
    event_policy_ref TEXT NOT NULL CHECK (event_policy_ref <> '' AND length(event_policy_ref) <= 256),
    last_valid_event_at TIMESTAMPTZ NOT NULL,
    retired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, bundle_id)
);
CREATE INDEX idx_source_map_bundle_retirements_project
    ON source_map_bundle_retirements (org_id, project_id, retired_at DESC);

-- +goose Down
DROP TABLE IF EXISTS source_map_bundle_retirements;
