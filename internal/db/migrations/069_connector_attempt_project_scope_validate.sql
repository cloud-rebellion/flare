-- +goose NO TRANSACTION
-- +goose Up
-- Migration 068 already enforces the paired scope on new writes. Repair older
-- misattributed receipts in small, committed batches so a large history does
-- not hold row locks or WAL in one startup transaction. The primary-key walk
-- visits each existing receipt once; a retry starts over and skips repaired
-- rows. New or changed rows cannot become mismatched after 068.
--
-- Goose must execute this DO as a top-level statement: PostgreSQL permits
-- COMMIT inside DO only outside an explicit transaction block. Each batch has
-- a five-second lock wait limit; a contended row fails the migration safely,
-- leaving completed batches committed for the next attempt.
-- +goose StatementBegin
DO $$
DECLARE
    cursor_id TEXT;
    batch_ids TEXT[];
    batch_end_id TEXT;
BEGIN
    LOOP
        IF cursor_id IS NULL THEN
            SELECT array_agg(id), max(id) INTO batch_ids, batch_end_id
            FROM (
                SELECT id FROM connector_attempts ORDER BY id LIMIT 500
            ) AS batch;
        ELSE
            SELECT array_agg(id), max(id) INTO batch_ids, batch_end_id
            FROM (
                SELECT id FROM connector_attempts
                WHERE id > cursor_id ORDER BY id LIMIT 500
            ) AS batch;
        END IF;
        EXIT WHEN batch_end_id IS NULL;

        PERFORM set_config('lock_timeout', '5s', true);
        UPDATE connector_attempts AS attempt
        SET project_id = NULL
        WHERE attempt.id = ANY(batch_ids)
          AND attempt.project_id IS NOT NULL
          AND NOT EXISTS (
              SELECT 1 FROM projects AS project
              WHERE project.id = attempt.project_id
                AND project.org_id = attempt.org_id
          );

        cursor_id := batch_end_id;
        COMMIT;
    END LOOP;
END $$;
-- +goose StatementEnd

-- Validation is a separate statement after all repair batches have committed.
-- It scans under PostgreSQL's lower-strength validation lock, allowing normal
-- connector writes to continue. Bound the DDL lock wait as well; if it cannot
-- start, Goose leaves this version unapplied and a rerun rechecks the history.
-- +goose StatementBegin
DO $$
BEGIN
    PERFORM set_config('lock_timeout', '5s', true);
    ALTER TABLE connector_attempts
        VALIDATE CONSTRAINT connector_attempts_project_org_fk;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Validation cannot be reversed without removing the constraint, which is
-- owned by 068's Down. The repair intentionally stays repaired.
SELECT 1;
