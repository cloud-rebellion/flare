-- +goose Up
-- Trace and span IDs are supplied by clients. Two projects may legitimately
-- report the same IDs and start time, so project_id must be part of the
-- partitioned table's uniqueness boundary. PostgreSQL propagates the new
-- primary key to existing partitions; start_time remains in the key because
-- it is the range partition column.
ALTER TABLE spans DROP CONSTRAINT spans_pkey;
ALTER TABLE spans ADD CONSTRAINT spans_pkey
    PRIMARY KEY (project_id, trace_id, span_id, start_time);

-- +goose Down
-- This rollback fails safely if two projects have already stored the same
-- trace/span/start tuple: PostgreSQL rolls back this migration transaction,
-- leaving the project-scoped key in place.
ALTER TABLE spans DROP CONSTRAINT spans_pkey;
ALTER TABLE spans ADD CONSTRAINT spans_pkey
    PRIMARY KEY (trace_id, span_id, start_time);
