-- name: InsertSentryClientReportDiscards :copyfrom
INSERT INTO sentry_client_report_discards
    (org_id, project_id, payload_sha256, reported_at, reason, category, quantity)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListSentryClientReportDiscards :many
SELECT reason, category, sum(quantity)::text AS quantity,
       count(DISTINCT payload_sha256)::bigint AS reports,
       min(received_at)::timestamptz AS first_received_at,
       max(received_at)::timestamptz AS last_received_at
FROM sentry_client_report_discards
WHERE org_id = $1 AND project_id = $2 AND received_at >= $3
GROUP BY reason, category
ORDER BY sum(quantity) DESC, reason, category
LIMIT 500;
