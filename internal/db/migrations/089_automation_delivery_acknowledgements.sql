-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Only the bounded acknowledgement is retained, atomically with delivery.
-- Historical delivered rows have no receiver proof and retain empty mode.
ALTER TABLE automation_alert_outbox
    ADD COLUMN receiver_mode TEXT NOT NULL DEFAULT '',
    ADD COLUMN receiver_http_status INTEGER,
    ADD COLUMN receiver_request_sha256 TEXT NOT NULL DEFAULT '',
    ADD COLUMN receiver_run_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN receiver_deduped BOOLEAN,
    ADD COLUMN receiver_acknowledged_at TIMESTAMPTZ,
    ADD CONSTRAINT automation_delivery_acknowledgement_shape CHECK (
        (receiver_mode = '' AND receiver_http_status IS NULL AND receiver_request_sha256 = ''
         AND receiver_run_id = '' AND receiver_deduped IS NULL AND receiver_acknowledged_at IS NULL)
        OR
        (state = 'delivered' AND delivered_at IS NOT NULL AND receiver_http_status IS NOT NULL AND receiver_http_status BETWEEN 200 AND 299
         AND receiver_request_sha256 ~ '^[0-9a-f]{64}$' AND receiver_acknowledged_at IS NOT NULL
         AND (
            (receiver_mode = 'http' AND receiver_run_id = '' AND receiver_deduped IS NULL)
            OR
            (receiver_mode = 'reactor-v1' AND (
                (event = 'test' AND receiver_run_id = '' AND receiver_deduped IS NULL)
                OR (event <> 'test' AND length(receiver_run_id) BETWEEN 1 AND 128
                    AND receiver_run_id ~ '^[A-Za-z0-9_.:-]+$' AND receiver_run_id NOT IN ('.','..')
                    AND receiver_deduped IS NOT NULL)
            ))
         ))
    );

CREATE INDEX automation_alert_outbox_delivery_page_idx
    ON automation_alert_outbox (org_id, project_id, created_at DESC, id DESC);
CREATE INDEX automation_alert_outbox_issue_page_idx
    ON automation_alert_outbox (org_id, project_id, issue_id, created_at DESC, id DESC);

-- +goose Down
DROP INDEX automation_alert_outbox_issue_page_idx;
DROP INDEX automation_alert_outbox_delivery_page_idx;
ALTER TABLE automation_alert_outbox
    DROP CONSTRAINT automation_delivery_acknowledgement_shape,
    DROP COLUMN receiver_mode,
    DROP COLUMN receiver_http_status,
    DROP COLUMN receiver_request_sha256,
    DROP COLUMN receiver_run_id,
    DROP COLUMN receiver_deduped,
    DROP COLUMN receiver_acknowledged_at;
