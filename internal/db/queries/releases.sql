-- name: UpsertRelease :exec
-- Get-or-create a release. Called from ingest (auto) and the deploy-marker
-- endpoint. created_at is preserved across calls (the first sighting wins).
INSERT INTO releases (id, project_id, org_id, version)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, version) DO NOTHING;

-- name: MarkReleaseDeploy :one
-- Only the authenticated deploy-marker endpoint may set deploy_marked_at.
-- Event ingest uses UpsertRelease and therefore cannot make an arbitrary old
-- event look like a deployed release.
INSERT INTO releases (id, project_id, org_id, version, deploy_marked_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (project_id, version) DO UPDATE
SET deploy_marked_at = now()
RETURNING version, created_at, deploy_marked_at;

-- name: TouchReleaseHeartbeat :execrows
-- Called only for the canonical info-level service-up:<service> heartbeat
-- shape after the event has been durably inserted. The project and org
-- predicates keep the timestamp tenant-safe even when release labels collide.
UPDATE releases
SET last_heartbeat_at = now()
WHERE project_id = $1 AND org_id = $2 AND version = $3;

-- name: ListReleasesByProject :many
-- Releases newest first, with the count of issues first seen in each.
SELECT r.version,
       r.created_at,
       (SELECT count(*) FROM issues i
         WHERE i.project_id = r.project_id AND i.org_id = r.org_id
           AND i.first_release = r.version) AS new_issues
FROM releases r
WHERE r.project_id = $1 AND r.org_id = $2
ORDER BY r.created_at DESC
LIMIT 200;

-- name: ListReleaseHealthByProject :many
-- Bounded release health for the authenticated project dashboard. Event counts
-- are scoped by both tenant and project and only include the requested hot-tier
-- window. A newly registered release with no events in the window is retained
-- when its deploy marker is recent, so a release that never received traffic
-- is visible as a zero-volume release. Raw event bodies are intentionally not
-- selected.
SELECT r.version,
       r.created_at,
       count(e.id)::bigint AS event_count,
       count(e.id) FILTER (WHERE e.level NOT IN ('info', 'debug'))::bigint AS error_count,
       count(DISTINCT e.issue_id) FILTER (
         WHERE e.issue_id IS NOT NULL AND e.level NOT IN ('info', 'debug')
       )::bigint AS affected_issue_count,
       min(e.received_at)::timestamptz AS first_seen,
       max(e.received_at)::timestamptz AS last_seen
FROM releases r
LEFT JOIN events e
  ON e.project_id = r.project_id
 AND e.org_id = r.org_id
 AND e.release = r.version
 AND e.received_at >= sqlc.arg(since)
WHERE r.project_id = sqlc.arg(project_id)
  AND r.org_id = sqlc.arg(org_id)
  AND (
    r.created_at >= sqlc.arg(since)
    OR EXISTS (
      SELECT 1
      FROM events recent
      WHERE recent.project_id = r.project_id
        AND recent.org_id = r.org_id
        AND recent.release = r.version
        AND recent.received_at >= sqlc.arg(since)
    )
  )
GROUP BY r.version, r.created_at
ORDER BY coalesce(max(e.received_at), r.created_at) DESC, r.version DESC
LIMIT sqlc.arg(row_limit);

-- name: ListReleaseFreshnessByProject :many
-- Explicit deploy markers are the admission boundary. A release created only
-- by an old event is intentionally absent, so freshness never infers a current
-- deployment from arbitrary historical traffic.
SELECT r.version,
       r.deploy_marked_at,
       r.last_heartbeat_at,
       EXTRACT(EPOCH FROM (now() - COALESCE(r.last_heartbeat_at, r.deploy_marked_at)))::double precision AS age_seconds
FROM releases r
WHERE r.project_id = sqlc.arg(project_id)
  AND r.org_id = sqlc.arg(org_id)
  AND r.deploy_marked_at IS NOT NULL
  AND r.deploy_marked_at >= sqlc.arg(since)
ORDER BY r.deploy_marked_at DESC, r.version DESC
LIMIT sqlc.arg(row_limit);

-- name: ListReleaseSymbolicationHealthByProject :many
-- Bounded release/source-map coverage for the authenticated project dashboard.
-- This reports whether an uploaded artifact can be selected for a stack frame;
-- it deliberately does not select source-map content, event payloads, or raw
-- stack traces. A frame is a candidate when it has a filename and a positive
-- generated line, matching the source-map resolver's input contract. Matching
-- uses both the complete filename and its slash-delimited basename because the
-- resolver accepts CDN URLs for artifacts uploaded by basename.
WITH scoped_releases AS (
    SELECT r.version, r.created_at
    FROM releases r
    WHERE r.project_id = sqlc.arg(project_id)
      AND r.org_id = sqlc.arg(org_id)
      AND (
        r.created_at >= sqlc.arg(since)
        OR EXISTS (
          SELECT 1
          FROM events recent
          WHERE recent.project_id = r.project_id
            AND recent.org_id = r.org_id
            AND recent.release = r.version
            AND recent.received_at >= sqlc.arg(since)
        )
      )
),
release_events AS (
    SELECT r.version,
           r.created_at,
           e.id AS event_id,
           e.received_at,
           e.stacktrace
    FROM scoped_releases r
    LEFT JOIN events e
      ON e.project_id = sqlc.arg(project_id)
     AND e.org_id = sqlc.arg(org_id)
     AND e.release = r.version
     AND e.received_at >= sqlc.arg(since)
),
frame_rollup AS (
    SELECT re.version,
           count(DISTINCT re.event_id)::bigint AS event_count,
           count(DISTINCT re.event_id) FILTER (
             WHERE jsonb_typeof(re.stacktrace -> 'frames') = 'array'
           )::bigint AS stacktrace_event_count,
           count(frame.value) FILTER (
             WHERE jsonb_typeof(frame.value -> 'filename') = 'string'
               AND frame.value ->> 'filename' <> ''
               AND frame.value ->> 'lineno' ~ '^[1-9][0-9]*$'
           )::bigint AS candidate_frame_count,
           count(frame.value) FILTER (
             WHERE jsonb_typeof(frame.value -> 'filename') = 'string'
               AND frame.value ->> 'filename' <> ''
               AND frame.value ->> 'lineno' ~ '^[1-9][0-9]*$'
               AND EXISTS (
                 SELECT 1
                 FROM source_map_artifacts sm
                 WHERE sm.project_id = sqlc.arg(project_id)
                   AND sm.org_id = sqlc.arg(org_id)
                   AND sm.release = re.version
                   AND sm.parse_status IN ('unvalidated', 'valid')
                   AND (
                     sm.name = frame.value ->> 'filename'
                     OR regexp_replace(sm.name, '^.*/', '') = regexp_replace(frame.value ->> 'filename', '^.*/', '')
                   )
               )
           )::bigint AS mapped_frame_count,
           min(re.received_at)::timestamptz AS first_seen,
           max(re.received_at)::timestamptz AS last_seen
    FROM release_events re
    LEFT JOIN LATERAL jsonb_array_elements(
      CASE
        WHEN jsonb_typeof(re.stacktrace -> 'frames') = 'array' THEN re.stacktrace -> 'frames'
        ELSE '[]'::jsonb
      END
    ) AS frame(value) ON true
    GROUP BY re.version
),
release_maps AS (
    SELECT sm.release,
           count(*)::bigint AS map_count,
           coalesce(sum(octet_length(sm.content)), 0)::bigint AS map_bytes
    FROM source_map_artifacts sm
    WHERE sm.project_id = sqlc.arg(project_id)
      AND sm.org_id = sqlc.arg(org_id)
    GROUP BY sm.release
)
SELECT r.version,
       r.created_at,
       coalesce(m.map_count, 0)::bigint AS map_count,
       coalesce(m.map_bytes, 0)::bigint AS map_bytes,
       coalesce(f.event_count, 0)::bigint AS event_count,
       coalesce(f.stacktrace_event_count, 0)::bigint AS stacktrace_event_count,
       coalesce(f.candidate_frame_count, 0)::bigint AS candidate_frame_count,
       coalesce(f.mapped_frame_count, 0)::bigint AS mapped_frame_count,
       (coalesce(f.candidate_frame_count, 0)::bigint - coalesce(f.mapped_frame_count, 0)::bigint)::bigint AS unmapped_frame_count,
       f.first_seen,
       f.last_seen
FROM scoped_releases r
LEFT JOIN frame_rollup f ON f.version = r.version
LEFT JOIN release_maps m ON m.release = r.version
ORDER BY coalesce(f.last_seen, r.created_at) DESC, r.version DESC
LIMIT sqlc.arg(row_limit);
