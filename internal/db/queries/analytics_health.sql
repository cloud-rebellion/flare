-- Global cold-tier exporter telemetry. The ledger has no tenant or provider
-- payload fields; this projection contains only bounded counts, byte/latency
-- aggregates, fixed error codes, and timestamps.
-- name: GetExportHealthStats :one
SELECT count(*) FILTER (WHERE status = 'succeeded')::bigint AS success_count,
       count(*) FILTER (WHERE status = 'failed')::bigint AS failure_count,
       count(*) FILTER (WHERE status = 'attempted')::bigint AS attempted_count,
       COALESCE(sum(exported_bytes) FILTER (WHERE status = 'succeeded' AND bytes_known), 0)::bigint AS exported_bytes,
       count(*) FILTER (WHERE status = 'succeeded' AND bytes_known)::bigint AS bytes_observations,
       COALESCE(avg(export_latency_ms) FILTER (WHERE status = 'succeeded'), 0)::double precision AS avg_latency_ms,
       COALESCE(max(export_latency_ms) FILTER (WHERE status = 'succeeded'), 0)::bigint AS max_latency_ms,
       max(attempted_at)::timestamptz AS latest_attempt_at,
       max(exported_at) FILTER (WHERE status = 'succeeded')::timestamptz AS latest_success_at,
       max(attempted_at) FILTER (WHERE status = 'failed')::timestamptz AS latest_failure_at,
       COALESCE((SELECT status FROM export_log ORDER BY attempted_at DESC LIMIT 1), 'unknown')::text AS latest_status,
       COALESCE((SELECT error_code FROM export_log WHERE status = 'failed' ORDER BY attempted_at DESC LIMIT 1), '')::text AS latest_error_code
FROM export_log;
