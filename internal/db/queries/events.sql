-- name: UpsertIssue :one
-- Groups an incoming event into its issue. is_new compares the returned id to
-- this insert's candidate id: first_seen == last_seen is wrong when multiple
-- events for one issue are written inside one transaction, because now() is
-- transaction-stable. Status
-- is intentionally left untouched on conflict so RETURNING exposes the PRIOR
-- status: the caller reopens a 'resolved' issue and fires a regression alert.
INSERT INTO issues (id, project_id, org_id, fingerprint, title, culprit, level, platform, first_release, first_seen, last_seen, event_count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), now(), 1)
ON CONFLICT (project_id, fingerprint) DO UPDATE
SET last_seen    = now(),
    event_count  = issues.event_count + 1,
    -- Level ESCALATES, never downgrades.
    --
    -- This used to be `level = EXCLUDED.level`, so the newest event decided the
    -- issue's severity outright. Ingest authenticates with a DSN public key that
    -- ships in a browser bundle, so anyone who can read a customer's page can
    -- post an event with the same fingerprint and level='info'. pageableLevel
    -- treats info/debug as never-page, so that one request silently switches off
    -- alerting for a live incident and nothing in the UI reports a change.
    --
    -- Unknown or empty maps to the rank of 'error', matching pageableLevel's
    -- "empty == error per Sentry semantics".
    level        = CASE
                     WHEN COALESCE(array_position(ARRAY['debug','info','warning','error','fatal'], lower(NULLIF(EXCLUDED.level, ''))), 4)
                        > COALESCE(array_position(ARRAY['debug','info','warning','error','fatal'], lower(NULLIF(issues.level,   ''))), 4)
                     THEN EXCLUDED.level
                     ELSE issues.level
                   END,
    -- Title and culprit are FIRST-WRITE-WINS, so a later event cannot rename an
    -- existing issue.
    --
    -- I originally kept these last-writer-wins and argued it was deliberate:
    -- they describe the current shape of the error and carry no automated
    -- consequence. Re-verification executed the attack and that argument does
    -- not survive it. Ingest authenticates with a DSN public key that ships in a
    -- browser bundle, so anyone who can load a customer's page can post an event
    -- on an existing fingerprint and turn "DatabaseError: connection refused"
    -- into anything they like. The issue stays open and alerting, and reads as
    -- something else entirely in the list, in alert bodies, and in the BYOAI
    -- triage prompt. The audit's own Fix says exactly this: "Keeping title as
    -- first-write-wins ... removes the alert/prompt vandalism primitive."
    --
    -- Nothing is lost operationally. The newest event's exception type and value
    -- are shown in full on the issue detail, so the current shape of the error is
    -- still one click away; only the GROUP's name is now stable.
    title        = issues.title,
    culprit      = issues.culprit
RETURNING id, project_id, org_id, fingerprint, title, culprit, level, status, platform,
          first_release, first_seen, last_seen, event_count, (id = $1) AS is_new;

-- ciguard:allow-no-project reopen by project-unique issue id
-- name: ReopenIssue :exec
UPDATE issues SET status = 'unresolved' WHERE id = $1 AND org_id = $2;

-- ciguard:allow-no-project count by project-unique issue id (spike rule window)
-- name: CountEventsForIssueSince :one
SELECT count(*) FROM events WHERE issue_id = $1 AND org_id = $2 AND received_at >= $3;

-- ciguard:allow-no-project atomic spike dedup by project-unique issue id
-- name: TrySetIssueSpike :execrows
-- Test-and-set: claims the spike alert for this issue only when the last one is
-- older than the window cutoff ($3). Returns 1 row when claimed, 0 when another
-- event already fired within the window.
UPDATE issues SET last_spike_at = now()
WHERE id = $1 AND org_id = $2 AND (last_spike_at IS NULL OR last_spike_at < $3);

-- name: InsertEvent :exec
INSERT INTO events (id, project_id, org_id, issue_id, level, message, exception_type, exception_value, platform, environment, release, stacktrace, payload, trace_id, span_id, received_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now());

-- name: ListIssues :many
-- Excludes info/debug-level signals (liveness heartbeats, breadcrumbs, health
-- check-ins) so the issues list shows incidents only, matching the overview
-- queries. Those signals still ingest (they keep a project "active" for the
-- watchdog) and remain openable by id; they just don't clutter the list.
SELECT * FROM issues
WHERE project_id = $1
  AND org_id = $2
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  -- ESCAPE '\' is a REDUNDANT restatement of PostgreSQL's default, kept as
  -- documentation. Measured 2026-08-11 against postgres:16-alpine with
  -- standard_conforming_strings=on: with the clause and without it, the term
  -- escapeLike("50%") produces identical results (matches the literal-50% row,
  -- does not match the other). Backslash is already the LIKE escape character
  -- when no ESCAPE clause is given.
  --
  -- The comment this replaces claimed the opposite, and the 2026-08-11 audit
  -- repeated it as finding M1's SQL half. That half was WRONG. The real defect
  -- was entirely Go-side: the logs search term got no escapeLike, no
  -- SanitizeText and no length cap, which is where all three of M1's named
  -- harms came from.
  --
  -- The clause stays because it makes the dependency on the escape character
  -- explicit rather than inherited, and internal/ciguard keeps both pillars
  -- spelling it the same way.
  AND (sqlc.narg(q)::text IS NULL
       OR title ILIKE '%' || sqlc.narg(q) || '%' ESCAPE '\'
       OR culprit ILIKE '%' || sqlc.narg(q) || '%' ESCAPE '\')
  AND level NOT IN ('info', 'debug')
-- id is the tiebreaker, and it is required: last_seen is not unique and it
-- MUTATES on every ingested event, so a bare ORDER BY last_seen with OFFSET
-- let rows shift between page requests, which silently duplicated some issues
-- across pages and skipped others entirely.
ORDER BY last_seen DESC, id DESC
LIMIT $3 OFFSET $4;

-- name: ListAllIssuesForExport :many
-- EVERY issue, including info/debug, for data portability. ListIssues carries
-- `level NOT IN ('info','debug')` so the dashboard shows incidents only, and
-- handleExport reused it: the bundle whose own comment says "data portability
-- means ALL of it" silently dropped every informational issue, with nothing in
-- the output saying so. Keyset paging on (last_seen, id), not OFFSET: last_seen
-- MUTATES on every ingested event, so an OFFSET walk under live ingest skips
-- rows and repeats others.
SELECT * FROM issues
WHERE project_id = $1
  AND org_id = $2
  AND (
    sqlc.narg(after_last_seen)::timestamptz IS NULL
    OR (last_seen, id) < (sqlc.narg(after_last_seen)::timestamptz, sqlc.narg(after_id)::text)
  )
ORDER BY last_seen DESC, id DESC
LIMIT $3;

-- name: CountIssues :one
-- MUST carry the same status/search predicates as ListIssues. It used to count
-- every issue regardless of filter, so the total shown beside the tabs
-- contradicted the list on every tab except "all", and paging computed from it
-- offered pages that were always empty.
SELECT count(*) FROM issues
WHERE project_id = $1 AND org_id = $2
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  -- ESCAPE '\' is a REDUNDANT restatement of PostgreSQL's default, kept as
  -- documentation. Measured 2026-08-11 against postgres:16-alpine with
  -- standard_conforming_strings=on: with the clause and without it, the term
  -- escapeLike("50%") produces identical results (matches the literal-50% row,
  -- does not match the other). Backslash is already the LIKE escape character
  -- when no ESCAPE clause is given.
  --
  -- The comment this replaces claimed the opposite, and the 2026-08-11 audit
  -- repeated it as finding M1's SQL half. That half was WRONG. The real defect
  -- was entirely Go-side: the logs search term got no escapeLike, no
  -- SanitizeText and no length cap, which is where all three of M1's named
  -- harms came from.
  --
  -- The clause stays because it makes the dependency on the escape character
  -- explicit rather than inherited, and internal/ciguard keeps both pillars
  -- spelling it the same way.
  AND (sqlc.narg(q)::text IS NULL
       OR title ILIKE '%' || sqlc.narg(q) || '%' ESCAPE '\'
       OR culprit ILIKE '%' || sqlc.narg(q) || '%' ESCAPE '\')
  AND level NOT IN ('info', 'debug');

-- ciguard:allow-no-project issue id is project-unique (one issue belongs to one project)
-- name: GetIssue :one
SELECT * FROM issues WHERE id = $1 AND org_id = $2;

-- ciguard:allow-no-project issue_id is project-unique, so events under it are one project's
-- name: ListEventsByIssue :many
SELECT * FROM events WHERE issue_id = $1 AND org_id = $2 ORDER BY received_at DESC LIMIT $3;

-- ciguard:allow-no-project issue_id is project-unique
-- name: GetLatestEvent :one
SELECT * FROM events WHERE issue_id = $1 AND org_id = $2 ORDER BY received_at DESC LIMIT 1;

-- ciguard:allow-no-project write by project-unique issue id
-- name: UpdateIssueStatus :one
UPDATE issues SET status = $3 WHERE id = $1 AND org_id = $2 RETURNING *;

-- ciguard:allow-no-project set-once flag by project-unique issue id
-- name: MarkIssueSensitive :exec
-- First detection wins: only set when not already flagged, so the badge is
-- stable and one write per issue, not per event.
UPDATE issues SET sensitive = $3 WHERE id = $1 AND org_id = $2 AND sensitive = '';
