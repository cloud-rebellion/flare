-- +goose NO TRANSACTION
-- +goose Up
-- The outbox references a channel in the same organization. Build the key
-- without a write-blocking table scan on a live notification table.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS notification_channels_id_org_outbox_key
    ON notification_channels (id, org_id);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = 'notification_channels_id_org_outbox_key'::regclass
          AND indrelid = 'notification_channels'::regclass
          AND indisvalid AND indisready AND indisunique AND indpred IS NULL
    ) THEN
        RAISE EXCEPTION 'notification_channels_id_org_outbox_key is invalid; drop it concurrently and rerun migration 071';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS notification_channels_id_org_outbox_key;
