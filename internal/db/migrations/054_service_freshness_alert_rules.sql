-- +goose Up

-- Service freshness rules monitor one exact canonical service-up heartbeat
-- dimension. A single release can carry heartbeats from several services, so
-- the release marker alone cannot prove that each component is alive.
ALTER TABLE alert_rules
    ADD COLUMN service_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN service_freshness_baseline_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_service_name_bound
    CHECK (octet_length(service_name) <= 64);

-- events is partitioned. Do not add a plain parent index here: migration-time
-- CREATE INDEX would lock every child partition and stall ingest. The existing
-- idx_events_project_received plus the time partition pruning bounds this
-- tenant/project-scoped freshness aggregate; release/service are residual
-- selectors inside that already bounded slice.

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_service_name_bound;
ALTER TABLE alert_rules
    DROP COLUMN IF EXISTS service_freshness_baseline_at,
    DROP COLUMN IF EXISTS service_name;
