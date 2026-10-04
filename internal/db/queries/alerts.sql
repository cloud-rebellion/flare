-- name: CreateAlertRule :one
INSERT INTO alert_rules (
    id, project_id, org_id, name, type, threshold, window_minutes, enabled,
    metric_name, comparison, threshold_value, span_name, percentile,
    connector_provider, connector_operation, connector_channel, connector_error_code, connector_connection_id, connector_sync_mode, release_version, service_name, log_severity,
    connector_quota_metric, usage_surface, usage_operation, usage_measure, api_key_hygiene_mode, ai_triage_mode, metric_labels, business_check_name
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30)
RETURNING *;

-- name: ListAlertRulesByProject :many
SELECT * FROM alert_rules WHERE project_id = $1 AND org_id = $2 ORDER BY created_at DESC;

-- name: ListEnabledAlertRulesByProject :many
SELECT * FROM alert_rules WHERE project_id = $1 AND org_id = $2 AND enabled = true;

-- name: CreateNotificationChannel :one
INSERT INTO notification_channels (id, org_id, type, config, enabled)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListNotificationChannelsByOrg :many
SELECT * FROM notification_channels WHERE org_id = $1 ORDER BY created_at DESC;

-- name: ListEnabledNotificationChannelsByOrg :many
SELECT * FROM notification_channels WHERE org_id = $1 AND enabled = true;

-- name: DeleteAlertRule :execrows
DELETE FROM alert_rules WHERE id = $1 AND project_id = $2 AND org_id = $3;

-- name: DeleteNotificationChannel :execrows
DELETE FROM notification_channels WHERE id = $1 AND org_id = $2;

-- name: GetNotificationChannel :one
SELECT * FROM notification_channels WHERE id = $1 AND org_id = $2;

-- name: CountEnabledNotificationChannelsByOrg :one
SELECT count(*) FROM notification_channels WHERE org_id = $1 AND enabled = true;

-- name: CountEnabledAlertRulesByOrg :one
SELECT count(*) FROM alert_rules WHERE org_id = $1 AND enabled = true;

-- name: RecordChannelDelivery :exec
UPDATE notification_channels
SET last_attempt_at = @attempted_at,
    last_ok_at = CASE WHEN @ok::boolean THEN @attempted_at ELSE last_ok_at END,
    last_error = CASE WHEN @ok::boolean THEN NULL ELSE @error_msg END
WHERE id = @id AND org_id = @org_id;

-- name: ListEnabledChannelsForProject :many
-- Channels that should receive an alert about ONE project: those routed to it,
-- plus every channel with no routing at all.
--
-- The NOT EXISTS clause is what makes "no rows means all projects" true, and it
-- is the whole backward-compatibility story: every channel that predates routing
-- has no rows, so it keeps receiving everything exactly as before.
SELECT c.* FROM notification_channels c
WHERE c.org_id = $1
  AND c.enabled = true
  AND (
    EXISTS (SELECT 1 FROM channel_projects cp WHERE cp.channel_id = c.id AND cp.project_id = $2)
    OR NOT EXISTS (SELECT 1 FROM channel_projects cp WHERE cp.channel_id = c.id)
  );

-- name: SetChannelEnabled :execrows
UPDATE notification_channels SET enabled = $3 WHERE id = $1 AND org_id = $2;

-- name: ReplaceChannelProjects :exec
-- ciguard:allow-unscoped scoped through the channel, whose org is checked by the caller before this runs; channel_projects has no org_id of its own
DELETE FROM channel_projects WHERE channel_id = $1;

-- name: AddChannelProject :exec
-- ciguard:allow-unscoped same: the project and channel are both org-verified by the handler before this runs
INSERT INTO channel_projects (channel_id, project_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: ListChannelProjects :many
-- ciguard:allow-unscoped read of a channel's routing; the channel is org-verified by the caller
SELECT project_id FROM channel_projects WHERE channel_id = $1;
