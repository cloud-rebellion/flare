-- +goose Up
-- An accepted bundle is an immutable, release-scoped source-map set. Its item
-- references protect admitted maps from deletion even if a caller bypasses the
-- HTTP delete handler. Content changes are rejected by the HTTP write boundary.
CREATE TABLE source_map_bundles (
    id             TEXT PRIMARY KEY,
    project_id     TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    org_id         TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    release        TEXT NOT NULL,
    digest         TEXT NOT NULL,
    artifact_count INTEGER NOT NULL CHECK (artifact_count > 0),
    total_bytes    BIGINT NOT NULL CHECK (total_bytes > 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, release)
);
CREATE INDEX idx_source_map_bundles_org_project ON source_map_bundles (org_id, project_id, created_at DESC);

CREATE TABLE source_map_bundle_items (
    bundle_id      TEXT NOT NULL REFERENCES source_map_bundles (id) ON DELETE CASCADE,
    -- Deferred NO ACTION blocks direct artifact erasure while permitting a
    -- project/org cascade to remove the bundle and its items in one tx.
    artifact_id    TEXT NOT NULL UNIQUE REFERENCES source_map_artifacts (id) ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED,
    content_sha256 TEXT NOT NULL,
    PRIMARY KEY (bundle_id, artifact_id)
);

-- +goose StatementBegin
CREATE FUNCTION reject_bound_source_map_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- A deferred trigger lets an explicit future bundle retirement remove the
    -- receipt and artifacts together, while a bare UPDATE cannot falsify a
    -- retained content hash or silently move the map to another release.
    IF (OLD.project_id, OLD.org_id, OLD.release, OLD.name, OLD.content, OLD.parse_status)
       IS DISTINCT FROM
       (NEW.project_id, NEW.org_id, NEW.release, NEW.name, NEW.content, NEW.parse_status)
       AND EXISTS (SELECT 1 FROM source_map_bundle_items WHERE artifact_id = OLD.id)
    THEN
        RAISE EXCEPTION 'bound source map is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER bound_source_map_immutable
AFTER UPDATE ON source_map_artifacts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION reject_bound_source_map_update();

-- +goose Down
DROP TRIGGER IF EXISTS bound_source_map_immutable ON source_map_artifacts;
DROP FUNCTION IF EXISTS reject_bound_source_map_update();
DROP TABLE IF EXISTS source_map_bundle_items;
DROP TABLE IF EXISTS source_map_bundles;
