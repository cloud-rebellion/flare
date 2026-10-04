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
   receipt, and a query for that same tenant and project. Arm an independent
   outside-in probe and paging destination for Flare itself; an unavailable
   Flare database cannot reliably page through Flare's own workers.
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
| Frontend build | Exact-commit CI passes syntax/type checks and a production bundle build. A real browser loads and executes that bundle, reaches a signed-in route, and reports a deliberate handled and unhandled canary. Static checks catch build-time syntax failures; HTTP asset availability alone does not prove browser execution. |
| Browser | A deployed release reports an uncaught error or rejection, failed request, and bounded performance/realtime series. The event, release, route template, and ingest receipt appear only in its project. A matching private source map resolves a minified frame without transmitting an arbitrary route or signing token. |
| Flutter | A real Android/iOS/web build reports framework, zone, handled, and returned HTTP errors with one release. Offline retry does not duplicate a logical event; the server receipt confirms acceptance. Upload and validate matching Dart/native symbols and prove frame resolution where supported. |
| Backend | A running process sends an error, trace, service heartbeat, and an explicit request-success or business metric. Confirm durable acceptance, tenant scope, SLO query and alert, release freshness, and a missing-heartbeat alert after shutdown. |
| SaaS | A registered installation has a server-observed, project- or organization-scoped identity. Verify account/auth, quota or rate-limit, failed operation, and last successful sync using provider responses; client-reported observations remain separately labeled and cannot impersonate the installation. |
| Business | A confidential project key posts an idempotent payload-free check and receipt. Verify an error changes the business-health query and alert while a public DSN cannot submit that check. |
| Flare control plane | An independent monitor sees public readiness and a canary's ingest-to-query journey, while a private scrape checks receipt failures and queue backlogs. A test alert reaches its actual email/webhook destination. A restored snapshot boots Flare and decrypts a harmless test secret. |
| AI and release | A fresh triage receipt produces a bounded evidence digest. A human session approves the proposed source revision. Stage-owned proof verifies CI for source `P`, merge from `P` to `M`, deployment of `M`, and a distinct post-cutover public synthetic observation of `M`; unverified claims do not advance. |

## Current proof boundary (2026-10-04)

The Flare, Stage, backup, Reactor, and first-party app changes described in this
plan are local release candidates. The Flare API code through migration 085
passed a focused PostgreSQL 16 suite for Sentry Store/envelope replay,
native logs, client connector attempts, OTLP, monitor admission/outbox, native
symbols, and HubSpot consent/refresh/revocation. A separate tenant-scope query
test passed against a fresh PostgreSQL 16 database. Go compilation, frontend
type check and production build, pinned SQL generation, and OpenAPI YAML/JSON
parity also passed. The Sentry atomic suite was updated to count the Store
event's new replay claim; its first integrated run exposed the stale expected
count, and the rerun passed. The backup Bash fixture passed locally. A
hidden-source-map build moved 45 JavaScript maps to a private build directory
and left zero maps in the served frontend directory. The build manifest binds
each public asset to its private map and the compiled DSN project identity.
Flare code candidate `53fbbf92a` fixes its Vite public-env-prefix
defect. A configured local build now proves that the DSN and full-commit
release occur in the same shipped JavaScript asset. Its manifest binds the
browser DSN origin and exact browser release. Focused manifest tests and a
configured build passed. That build used a dummy DSN and a precommit release;
it is local build evidence, not an exact-final-commit image or live event.
A fixed handled dashboard canary is in a real immutable chunk; its local
hidden map resolves the generated error to `src/lib/flare-client.ts:362`.
Nine focused browser-client tests and the frontend type check passed. The
four older example RUM route-scrubbing assertions were then corrected for the
fixed route-word allowlist: opaque segments and emails become `:id`, while
`orders`, `events`, and `stream` remain fixed route labels. The focused example
and dashboard suites now pass 14 tests; frontend type check also passes. A
configured hidden-map rebuild reconfirmed the canary mapping to line 362 with
the same synthetic build inputs.
Stage candidate `742f9632e` has a disabled-by-default Flare map gate. It
requires the browser DSN origin and release to match the authenticated project
and running image. It compares the
content-addressed final-image manifest with the live container, probes the
reviewed browser ingest hostname for exact JSON readiness, and hashes public
scripts and private-map fallbacks on both reviewed dashboard hosts before any
upload. It then uploads and reads back both sets of opaque asset names, with
cleanup before its success receipt. Focused local tests passed. At about 45
maps per host, a release consumes about 90 of the project's 2,000 artifact
slots; retention needs review. Flare's artifact upload/list sizes and quota
totals count stored UTF-8 bytes rather than Unicode characters. Quota
admission now locks the project, accounts for the exact replacement row, and
commits the quota check and upsert in one transaction. Focused Unicode,
concurrent upload, retry-at-cap, byte-delta, and foreign-tenant regressions
passed against disposable PostgreSQL 16 after migrations through 085. Stage
still needs a before-cutover admission gate and receipt-aware retention that
protects live, rollback, and recent-event releases. A Stage-only quota estimate
cannot guarantee capacity: manifests omit final stored sizes, the server
sanitizes map bytes, and concurrent uploads can race a read-only list. Moving
today's individual uploads before cutover is also unsafe: a later 507 can
strand a partial release, while artifact-ID deletion can remove a replacement.
Flare needs an atomic bundle reservation or equivalent conditional lifecycle
API bound to project, release, and map digests before Stage can gate cutover.
The current upload runs after cutover, so quota exhaustion can leave a live
release with a failed deployment run. None of this establishes production
capture.

The live backup unit was observed failing on the removed `hephaestus-core`
container. A correctly configured read-only Restic query found zero
Stage-tagged off-host snapshots. The repaired backup candidate preserves the
installed TrustIssues WAL-safe snapshot and has a networkless regression
fixture. The latest local Stage bundle (`20261004-211224.766334068`) passed a
network-isolated restore, repository fsck, and offline `demo-local` smoke;
evidence is retained at `/var/tmp/stage-restore-vQO19CpK` on the production
host. This establishes a local Stage restore only. The backup-script repair,
fresh off-host Stage and separate-secret snapshots, and an independent Flare
database/secret restore remain required before cutover.

The ten first-party browser bridges are source-wired, but public bundle
sampling did not prove an active Flare DSN or event receipt. Most app builds
also lack a demonstrated private-map artifact and most Compose releases still
default to `development`; each app needs exact build-time release injection,
private map delivery, and an event and bundled-frame resolution canary on its
own product branch. BrightCRM combined candidate `9a5a3bfbf` and Atomicsite candidate
`4f01a14a8` now prove in configured local builds that a synthetic DSN and full
source SHA reach the shipped JavaScript, while 233 and 182 JavaScript maps,
respectively, stay private. The initial configured builds exposed a real Vite
env-prefix bug that silently disabled both bridges; the corrected manifest
builds fail if configured telemetry values are absent from compiled JavaScript.
The Atomicsite handled canary is compiled into a real immutable chunk; its
local hidden map resolves the generated error position to
`src/lib/flare-client.ts:322`. Stage candidate `742f9632e` binds both pilot
builds to the checked-out commit and provisioned Flare project before image
build. Its disabled-by-default Atomicsite cutover gate checks the final image,
private rebuild, running container, and public asset route before authenticated
map upload and readback. The new disabled-by-default BrightCRM cutover gate
also binds the final image, live container, private frontend rebuild, and exact
public script hashes before upload. It requires the reviewed 19-byte public
404 response for private map paths and reads back each accepted artifact.
Both app gates accept only the two reviewed HTTPS Flare origins and compare
the selected origin's JSON readiness with the canonical API host before the
first upload. A canceled cutover attempts bounded detached cleanup for private
Docker artifacts. The focused local Stage map and secret-inventory suites
passed, but neither gate has run against a final image or live Flare. A
deployed browser event and resolved frame, alert delivery, and rollback still
need proof. Flare's 2,000-artifact per-project quota needs a retention policy
before repeated 182-map Atomicsite or 233-map BrightCRM releases. Current
host checks found both app containers healthy and their public `/login` routes
returning 200; Atomicsite is the narrower first pilot, subject to the release
gates. Bright Hub (`portal`) was previously marked
discarded and must be revalidated before any rollout. A
Flutter starter has framework, zone, platform, HTTP, frame, and metric hooks,
but no production Flutter app repository or Dart/Flutter build has been
selected. Backend DSNs and provider connections still need deployed
configuration and canaries. HubSpot's account probe and explicit project
binding exist locally. Browser-bound OAuth consent, encrypted refresh, and
provider revocation are implemented and tested; a real HubSpot app registration,
consent, account probe, and alert remain unverified. Legacy installations still
require an admin to bind them. The connector sync watchdog now detects a bound
active installation whose first sync never ran, after its rule-window grace;
the project/provider/tenant and paused/revoked exclusions passed a disposable
PostgreSQL 16 regression. A rejected HubSpot refresh grant now yields a
payload-free, project-bound `auth_expired` check receipt and worker result;
the on-demand handler regression proved that a matching alert fires and that
foreign tenants cannot check the installation. A failed receipt write remains
visible, and token-store infrastructure errors do not impersonate the provider.
An enabled scheduler, real HubSpot consent, and a live failed-grant pass are
still needed to prove those alerts in production.

The BrightCRM native metric pilot separates accepted metric batches from
maintenance check-ins, so a healthy check-in cannot mask a stalled metric
batch. It reuses one batch key and exact body on retry;
the Flare candidate commits metric rows and their terminal receipt together
and replays the result for seven days. Native logs and client connector-attempts
have the same optional key contract. Sentry envelope items use a project-scoped
replay ledger; Sentry Store events with a supplied event ID share that ledger
and commit it with the event and terminal receipt. Store events without an ID
remain fresh events on each submission. Replay protection expires with the
configured hot telemetry retention window and requires byte-identical payloads.
OTLP rows and their terminal receipt commit atomically, but stock exporters
cannot supply a new per-batch key through a static header setting. A keyless
ambiguous retry can duplicate data. A custom OTLP/HTTP sender can retain a
fresh key with each exact batch for seven-day replay protection; acceptance
needs keyed logs, traces, and metrics canaries plus a separate ordinary
keyless collector canary that checks partial-success handling. Older Flare
servers and a BrightCRM process restart before delivery still need separate
failure handling. Native symbol upload acceptance does not itself prove frame
resolution.

The public Flare `/metrics` probe and `/readyz` currently return the SPA shell
at the live hostname, so a successful HTTP status is not a scrape or readiness
proof. The preserved Flare `BASE_URL` is `https://flare.example.com`;
both that hostname and `https://flare.cloudrebellion.tech` currently serve an
HTML shell at `/readyz`. The new Stage map gate accepts only these reviewed
browser destinations and requires exact release JSON before upload. Stage's
public synthetic verifies the shell, immutable assets, readiness, and release
identity. Its new opt-in Chromium mode also exercises the anonymous login
form and emits a distinct version 3 browser receipt; the runtime could not be
run against a local Docker daemon, so it remains disabled pending a real
post-cutover trial. A signed-in browser journey remains an additional live
acceptance check. The exact external receipt verifier requires a scoped Stage
profile and live proof; no caller-provided success flag is sufficient. A
synthetic Stage proof was rejected at Flare promotion because its expected
pipeline was empty even though the verifier bound it to the deploy pipeline.
The candidate now uses the bound deploy pipeline in both places; focused
PostgreSQL-backed receipt promotion passed. No real Stage attestation has been
promoted from a deployed release.

A Flare-owned outside-in canary candidate now checks the public `/readyz` against
an operator-pinned release, posts an idempotent native log, reads its retained
receipt and exact log from a dedicated project, and alerts through an
independent SMTP route. Its offline tests passed, but it is disabled and has
not been installed on an independent host or proven to reach an inbox. A
separate deadman must cover loss of that host or its timer.

The Reactor candidate accepts a signed, project-bound Flare notice and starts
a durable run, but it does not yet fetch fresh incident evidence or prepare a
code fix. Its current authoring path activates newly registered workflows,
and a signal token alone does not authenticate a human reviewer. A safe
remediation workflow must be reviewed before activation, fetch evidence from
a fixed Flare origin, bind a proposed source commit to that evidence, and
require Flare's authenticated human `review:approved` receipt before CI.
Keep proposal workers out of Stage merge and deploy tools: a product-scoped
Stage merge can schedule production deployment. Stage-owned exact CI, merge,
deploy, and post-cutover synthetic receipts must then verify the same source
and image chain. No end-to-end AI remediation run has been accepted.

Keep the rollout ledger explicit: `local`, `pushed`, `exact CI`, `merged`,
`deployed`, `live accepted`, and `restored` are separate states for every
product and integration.
