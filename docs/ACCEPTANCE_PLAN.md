# Flare acceptance plan

Flare is complete for an application only when a real signal from its deployed
release is accepted, queryable in the correct project, capable of triggering a
configured alert, and recoverable after a failed deployment. Source wiring and
green tests are necessary evidence, but they are not live acceptance.

## Release order

1. Restore the backup path on the production host. Use Stage's exact-path
   managed branch for the backup script, install the merged bytes with the
   existing root-owned mode, run the full backup unit, inspect fresh primary and
   separate-secret snapshots, and perform an isolated Stage and Flare restore.
2. Merge and deploy Stage's Flare migration, source-identity, receipt-attestation,
   and optional public synthetic gates. Record its exact CI, merge, deployment,
   image, and smoke receipts. Enable the public synthetic step only after the
   public Flare route and runner egress are verified.
3. Merge and deploy Flare. Require migrations, `/readyz` JSON with the expected
   source commit and release, a public shell, a real stored event and ingest
   receipt, and a query for that same tenant and project.
4. Roll out each first-party app through its own product branch. Supply its
   public DSN, environment, and immutable release at build time. Send a benign
   browser or backend canary and verify the accepted event and receipt in Flare.
   Upload release source maps or mobile symbols through the authenticated API.
5. Connect each external SaaS installation through a reviewed credential or
   consent flow. Run a server-observed check, verify the tenant and installation
   binding, then exercise quota, auth-expiry, retry, and sync-lag alert rules.
6. Enable the project-bound signed automation channel and reviewed receiver.
   Confirm its `test` event is a no-op, then a real issue yields a durable
   delivery, bounded fresh AI evidence, human review, and exact Stage-verified
   CI, merge, deploy, and post-cutover synthetic receipts.

## Evidence required for each signal class

| Class | Acceptance evidence |
| --- | --- |
| Browser | A deployed release reports an uncaught error or rejection, failed request, and bounded performance/realtime series. The event, release, route template, and ingest receipt appear only in its project. A matching private source map resolves a minified frame without transmitting an arbitrary route or signing token. |
| Flutter | A real Android/iOS/web build reports framework, zone, handled, and returned HTTP errors with one release. Offline retry does not duplicate a logical event; the server receipt confirms acceptance. Upload and validate matching Dart/native symbols and prove frame resolution where supported. |
| Backend | A running process sends an error, trace, service heartbeat, and an explicit request-success or business metric. Confirm durable acceptance, tenant scope, SLO query and alert, release freshness, and a missing-heartbeat alert after shutdown. |
| SaaS | A registered installation has a server-observed, project- or organization-scoped identity. Verify account/auth, quota or rate-limit, failed operation, and last successful sync using provider responses; client-reported observations remain separately labeled and cannot impersonate the installation. |
| Business | A confidential project key posts an idempotent payload-free check and receipt. Verify an error changes the business-health query and alert while a public DSN cannot submit that check. |
| AI and release | A fresh triage receipt produces a bounded evidence digest. A human session approves the proposed source revision. Stage-owned proof verifies CI for source `P`, merge from `P` to `M`, deployment of `M`, and a distinct post-cutover public synthetic observation of `M`; unverified claims do not advance. |

## Current proof boundary (2026-10-04)

The Flare, Stage, backup, Reactor, and first-party app changes described in this
plan are local release candidates. Combined Flare PostgreSQL 16 API and query
tests through migration 079, focused Go tests, frontend type check/build,
pinned SQL generation, and the backup Bash fixture have passed locally. A
hidden-source-map build moved 45 JavaScript maps to a private build directory
and left zero maps in the served frontend directory. These checks do not
establish production capture. The live backup unit was observed failing on the
removed `hephaestus-core` container, and a Stage verified-bundle off-host
snapshot was missing; repair and an independent restore are required before
cutover. The repaired backup candidate preserves the installed TrustIssues
WAL-safe snapshot and has a networkless regression fixture.

The ten first-party browser bridges are source-wired, but public bundle
sampling did not prove an active Flare DSN or event receipt. Their new private
asset IDs still need app-specific rollout and a live upload/resolution canary.
A Flutter starter exists without a selected production app repository. Backend
DSNs and provider connections still need deployed configuration and canaries.
HubSpot's initial account probe and explicit project binding exist locally;
legacy installations require an admin to bind them, while consent and broader
provider workflows require separate account authorization. The BrightCRM
native metric pilot can duplicate a delta after an ambiguous HTTP failure, so
its delivery is not yet an exactly-once metric contract. Native-symbol upload
acceptance does not itself prove frame resolution. Stage's public synthetic
verifies the shell, immutable assets, readiness, and release identity, while a
signed-in browser journey remains an additional live acceptance check. The
exact external receipt verifier requires a scoped Stage profile and live proof;
no caller-provided success flag is sufficient.

Keep the rollout ledger explicit: `local`, `pushed`, `exact CI`, `merged`,
`deployed`, `live accepted`, and `restored` are separate states for every
product and integration.
