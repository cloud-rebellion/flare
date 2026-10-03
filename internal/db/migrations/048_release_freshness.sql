-- +goose Up

-- A release row is also created automatically when an event first reports a
-- version. Keep the deploy marker separate so a stale-release rule never
-- treats an arbitrary old event as a deployed release.
ALTER TABLE releases
    ADD COLUMN deploy_marked_at TIMESTAMPTZ;

-- Runtime liveness is an explicit info-level `service-up:<service>` event
-- carrying the release value. Store only its timestamp; event bodies remain in
-- the normal hot tier and are never returned by freshness queries.
ALTER TABLE releases
    ADD COLUMN last_heartbeat_at TIMESTAMPTZ;

CREATE INDEX idx_releases_freshness
    ON releases (project_id, org_id, deploy_marked_at DESC)
    WHERE deploy_marked_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_releases_freshness;
ALTER TABLE releases DROP COLUMN IF EXISTS last_heartbeat_at;
ALTER TABLE releases DROP COLUMN IF EXISTS deploy_marked_at;
