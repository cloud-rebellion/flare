-- +goose Up

-- Metric threshold rules may select a bounded set of low-cardinality labels
-- (for example environment=production or route=/checkout). The selector is
-- stored separately from metric_name so a stable RUM series can still be
-- alerted independently per client, release, or route.
ALTER TABLE alert_rules
    ADD COLUMN metric_labels JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_metric_labels_object
    CHECK (jsonb_typeof(metric_labels) = 'object' AND octet_length(metric_labels::text) <= 1024);

-- +goose Down
ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_metric_labels_object;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS metric_labels;
