-- +goose Up

-- Connector freshness rules need a durable point from which to measure the
-- first cadence.  Keeping this separate from created_at makes the baseline
-- explicit in the schema and leaves room for a future operator reset without
-- inferring a baseline from connector traffic.  Existing rows are harmless:
-- only the connector_freshness rule type reads this value.
ALTER TABLE alert_rules
    ADD COLUMN connector_freshness_baseline_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE alert_rules DROP COLUMN IF EXISTS connector_freshness_baseline_at;
