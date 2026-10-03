-- name: ListClientHealthByProject :many
-- Bounded client/runtime health for a project. The dimensions come from the
-- SDK's platform, environment, and release fields; they let an operator tell
-- a browser, Flutter, and backend regression apart without returning payloads.
-- Counts are scoped by both tenant and project and remain on the requested hot
-- window. The caller caps the number of dimension groups returned.
SELECT e.platform,
       e.environment,
       e.release,
       count(*)::bigint AS event_count,
       count(*) FILTER (WHERE e.level NOT IN ('info', 'debug'))::bigint AS error_count,
       count(DISTINCT e.issue_id) FILTER (
         WHERE e.issue_id IS NOT NULL AND e.level NOT IN ('info', 'debug')
       )::bigint AS affected_issue_count,
       min(e.received_at)::timestamptz AS first_seen,
       max(e.received_at)::timestamptz AS last_seen
FROM events e
WHERE e.project_id = sqlc.arg(project_id)
  AND e.org_id = sqlc.arg(org_id)
  AND e.received_at >= sqlc.arg(since)
GROUP BY e.platform, e.environment, e.release
ORDER BY error_count DESC, event_count DESC, last_seen DESC,
         e.platform, e.environment, e.release
LIMIT sqlc.arg(row_limit);
