-- Payload-free tenant usage counters. Never add secrets, request bodies, URLs,
-- user identifiers, IP addresses, or provider response text here.

-- name: UpsertUsageBucket :exec
INSERT INTO usage_buckets (
    org_id, project_id, credential_id, surface, operation, bucket_start,
    request_count, error_count, denied_count
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (org_id, project_id, credential_id, surface, operation, bucket_start)
DO UPDATE SET request_count = usage_buckets.request_count + EXCLUDED.request_count,
              error_count = usage_buckets.error_count + EXCLUDED.error_count,
              denied_count = usage_buckets.denied_count + EXCLUDED.denied_count;

-- name: ListUsageBucketsByOrg :many
SELECT CAST(NULLIF(project_id, '') AS text) AS project_id,
       CAST(NULLIF(credential_id, '') AS text) AS credential_id,
       surface,
       operation,
       sum(request_count)::bigint AS request_count,
       sum(error_count)::bigint AS error_count,
       sum(denied_count)::bigint AS denied_count,
       min(bucket_start)::timestamptz AS first_seen_at,
       max(bucket_start)::timestamptz AS last_seen_at
FROM usage_buckets
WHERE org_id = $1
  AND bucket_start >= $2
GROUP BY project_id, credential_id, surface, operation
ORDER BY last_seen_at DESC, surface, operation
LIMIT $3;
