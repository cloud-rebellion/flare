# Source-map bundle retirement gate

Flare has a transactional retirement route for immutable source-map bundles:
`DELETE /api/projects/{id}/artifact-bundles/{bundleID}`. It requires an
interactive administrator and `If-Match: "sha256:<64 lowercase hex digits>"`
from the bundle receipt. A successful response returns a payload-free
retirement receipt. `GET /api/projects/{id}/artifact-bundle-retirements/{id}`
reads it back with ordinary project-scoped authentication. An exact repeat of
the DELETE returns the original receipt without deleting anything else.

**The route is intentionally disabled for deletion today.** It returns `503`
because no trusted release-protection provider is wired. The release registry
is created by an event or a member-supplied deploy marker and does not identify
what is currently live or retained for rollback. Stage's verified automation
receipts bind issue, revision, and run, but not the browser `release` label used
by source maps, every live deployment target, or its rollback target. A user
supplied release list or old deploy receipt cannot authorize erasure.

## Stage producer required to enable retirement

The private `sourceMapRetirementFencer.Acquire(ctx, orgID, projectID, release)`
adapter must obtain a trusted, exclusive project-wide Stage lease and return
one complete `sourceMapReleaseProtection` value:

- Exact Flare organization, project, and candidate release IDs, a durable
  Stage lease/proof reference, a deployment generation, and a fresh capture
  time.
- The exact browser release label on **every** live target of the project, and
  every configured rollback candidate. Empty lists must be explicit; missing
  targets or unknown release labels are errors. The labels must match the
  `source_map_bundles.release` value, not just a Git SHA or image tag.
- A durable, trusted `event_policy_ref` and `last_valid_event_at` earlier than
  the start of the 30-day quiet window. This must come from an enforced client
  lifetime or ingest coordination policy proving that a valid old-release
  event can no longer arrive. A lack of events in Flare alone is insufficient.
- A `ReleaseFence` function that relinquishes the Stage lease only after Flare's
  transaction commits or rolls back. Stage must fence all deploy and rollback
  operations on those targets for the entire lease. A snapshot that can become
  stale while a deployment advances is insufficient.

The adapter must authenticate Stage and bind its response to the configured
tenant, repository, program, project, and deployment generation. Stage outages,
expired leases, ambiguous target inventory, absent rollback state, any
unmapped release, or unbounded old-client/event lifetime return no proof. The
HTTP caller supplies only the bundle ID and digest precondition; it cannot
supply or override the protection sets.
Wire the adapter in `NewServer` only after Stage exposes this lease and exact
release inventory. Keep the field nil until then.

With a valid lease held, Flare locks the tenant's project row, loads the exact
bundle, and rejects deletion if the release is live or a rollback candidate.
It also rejects a bundle less than 30 days old, an event received in the last
30 days, or a deploy marker/heartbeat in that window. If configured hot-event
retention is under 30 days, it refuses deletion because the recent-event check
cannot be trusted. It verifies the stored item count and byte total, deletes
the exact maps and bundle, and inserts the retirement receipt and audit row in
one PostgreSQL transaction. Any failure leaves the maps intact.

The event check describes retained, accepted events at the retirement
decision. It cannot prevent concurrent or later ingestion by itself. The
trusted bounded-lateness policy must establish when old clients stop being
valid, and the Stage lease must keep the deployment state stable through the
transaction. If either proof is unavailable, the route stays at `503`.

Responses: `403` for a non-session caller, `404` for a foreign or absent
project/bundle, `428` for missing `If-Match`, `412` for a changed digest,
`503` for missing trusted proof or insufficient hot retention, `409` for a
protected/recent release, and `200` with the retirement receipt for a commit
or exact replay. The single-map DELETE route cannot delete a bundled map.

Focused PostgreSQL 16 tests cover fail-closed behavior, tenant and session
boundaries, live/rollback/recent protection, exact digest, transactional
receipt/audit commit, idempotent readback, and rollback after audit failure.
These tests inject an internal fake fencer; they do not prove a real Stage
lease exists.
