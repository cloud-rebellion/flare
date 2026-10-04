-- +goose NO TRANSACTION
-- +goose Up

-- The referenced key is built without blocking issue ingest on a large live
-- table. A failed concurrent build leaves an invalid index, so require a
-- valid unique key before creating the queue's composite foreign key.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS issues_id_org_project_evaluation_key
    ON issues (id, org_id, project_id);
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = 'issues_id_org_project_evaluation_key'::regclass
          AND indrelid = 'issues'::regclass
          AND indisvalid AND indisready AND indisunique AND indpred IS NULL
    ) THEN
        RAISE EXCEPTION 'issues_id_org_project_evaluation_key is invalid; drop it concurrently and rerun migration 074';
    END IF;
END $$;
-- +goose StatementEnd

-- The event and its alert-evaluation handoff commit together. Keep the queue
-- payload-free: the worker reloads the current issue under the same org/project
-- scope, while these booleans preserve the event's transition identity.
CREATE TABLE issue_alert_evaluations (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    issue_id        TEXT NOT NULL,
    is_new          BOOLEAN NOT NULL,
    reopened        BOOLEAN NOT NULL,
    event_count     BIGINT NOT NULL CHECK (event_count BETWEEN 0 AND 1000000000),
    observed_at     TIMESTAMPTZ NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'leased', 'done', 'dead')),
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 12),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token     TEXT NOT NULL DEFAULT '',
    leased_until    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT issue_alert_evaluations_project_scope_fk
        FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE,
    CONSTRAINT issue_alert_evaluations_issue_scope_fk
        FOREIGN KEY (issue_id, org_id, project_id) REFERENCES issues (id, org_id, project_id) ON DELETE CASCADE,
    CONSTRAINT issue_alert_evaluations_lease_shape CHECK (
        (state = 'leased' AND lease_token <> '' AND leased_until IS NOT NULL)
        OR (state <> 'leased' AND lease_token = '' AND leased_until IS NULL)
    )
);

CREATE INDEX issue_alert_evaluations_due_idx
    ON issue_alert_evaluations (next_attempt_at, id) WHERE state = 'pending';
CREATE INDEX issue_alert_evaluations_oldest_pending_idx
    ON issue_alert_evaluations (created_at, id) WHERE state = 'pending';
CREATE INDEX issue_alert_evaluations_expired_idx
    ON issue_alert_evaluations (leased_until, id) WHERE state = 'leased';
CREATE INDEX issue_alert_evaluations_dead_idx
    ON issue_alert_evaluations (created_at, id) WHERE state = 'dead';
CREATE INDEX issue_alert_evaluations_done_prune_idx
    ON issue_alert_evaluations (updated_at, id) WHERE state = 'done';
CREATE INDEX issue_alert_evaluations_project_idx
    ON issue_alert_evaluations (org_id, project_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS issue_alert_evaluations;
DROP INDEX CONCURRENTLY IF EXISTS issues_id_org_project_evaluation_key;
