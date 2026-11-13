-- +goose Up
-- ALTER TABLE takes ACCESS EXCLUSIVE on orgs. Without a deadline it queues
-- behind any open transaction that read orgs (every authenticated request)
-- and every read behind it stalls too, which on a rolling deploy is the
-- serving pod. Give up after 5s and let the deploy retry, like 089-091.
SET LOCAL lock_timeout = '5s';

-- The operator workspace is the org that runs this instance: it owns the
-- instance-wide settings (data retention) and receives Flare's own security
-- events. It used to be "whichever org is oldest", which made it an accident
-- of deletion order: owner A deleting the first org handed instance-wide
-- telemetry deletion to the next-oldest tenant and erased the retention audit
-- trail with it. Now it is an explicit flag, at most one org holds it, and
-- DELETE /api/org refuses to remove it while other workspaces exist.
--
-- Upgrades flag the current oldest org (id breaks a created_at tie). A fresh
-- install flags the first org created (CreateOrg claims the flag when no org
-- holds it).
ALTER TABLE orgs ADD COLUMN is_operator BOOLEAN NOT NULL DEFAULT FALSE;
CREATE UNIQUE INDEX orgs_single_operator ON orgs (is_operator) WHERE is_operator;
UPDATE orgs SET is_operator = TRUE
WHERE id = (SELECT id FROM orgs ORDER BY created_at ASC, id ASC LIMIT 1);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP INDEX IF EXISTS orgs_single_operator;
ALTER TABLE orgs DROP COLUMN IF EXISTS is_operator;
