-- +goose Up
-- Instance-wide telemetry retention chosen in the dashboard. The partitioned
-- telemetry tables and the Parquet cold tier are shared by every org on the
-- instance, so this is one row, not one per tenant: a CHECK pins the key to
-- TRUE. With no row stored, RETENTION_DAYS / COLD_RETENTION_DAYS remain the
-- effective values, which is how an upgraded install behaves until an admin
-- saves something.
--
-- cold_mode:
--   forever  archive to Parquet before the hot drop, never prune the archive
--            (the behaviour before this table existed)
--   days     archive, then prune archive days older than cold_retention_days
--   off      drop hot partitions WITHOUT exporting them. Never a default: it
--            can only be stored by an explicit choice.
--
-- The bounds mirror internal/retention. hot >= 7 is not taste: ingest accepts
-- timestamps up to 7 days old (ingest.MaxBackfillDays) and the partition
-- manager pre-creates that backfill window every pass, so a shorter hot window
-- would recreate and then drop partitions that ingest is still writing into.
CREATE TABLE retention_settings (
    singleton           BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    hot_retention_days  INTEGER NOT NULL CHECK (hot_retention_days BETWEEN 7 AND 365),
    cold_mode           TEXT NOT NULL CHECK (cold_mode IN ('forever', 'days', 'off')),
    cold_retention_days INTEGER,
    updated_by          TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT retention_settings_cold_days_check CHECK (
        (cold_mode = 'days' AND cold_retention_days IS NOT NULL
         AND cold_retention_days BETWEEN hot_retention_days AND 3650)
        OR (cold_mode <> 'days' AND cold_retention_days IS NULL)
    )
);

-- +goose Down
DROP TABLE IF EXISTS retention_settings;
