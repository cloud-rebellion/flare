-- +goose Up
-- Only fixed receiver-owned reason codes and bounded counts. Never persist
-- arbitrary SDK item types, parser messages, payloads or credentials here.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION flare_valid_ingest_rejections(counts JSONB)
RETURNS BOOLEAN LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT CASE WHEN jsonb_typeof(counts) <> 'object' THEN false ELSE
        counts - ARRAY[
            'invalid_envelope', 'invalid_event', 'invalid_event_id',
            'invalid_transaction', 'invalid_transaction_id', 'invalid_client_report',
            'unsupported_session', 'unsupported_attachment', 'unsupported_profile',
            'unsupported_check_in', 'unsupported_log', 'unsupported_span',
            'unsupported_metric', 'unsupported_other', 'identity_conflict', 'storage_failed'
        ]::TEXT[] = '{}'::JSONB
        AND NOT EXISTS (
            SELECT 1 FROM jsonb_each(counts) AS item
            WHERE jsonb_typeof(item.value) <> 'number'
               OR item.value::TEXT !~ '^([1-9][0-9]{0,2}|1000)$'
        ) END;
$$;
-- +goose StatementEnd

ALTER TABLE ingest_receipts
    ADD COLUMN rejection_counts JSONB NOT NULL DEFAULT '{}'::JSONB,
    ADD CONSTRAINT ingest_receipts_rejection_counts_bounded
        CHECK (flare_valid_ingest_rejections(rejection_counts)) NOT VALID;
ALTER TABLE ingest_receipts VALIDATE CONSTRAINT ingest_receipts_rejection_counts_bounded;

-- +goose Down
ALTER TABLE ingest_receipts DROP COLUMN rejection_counts;
DROP FUNCTION flare_valid_ingest_rejections(JSONB);
