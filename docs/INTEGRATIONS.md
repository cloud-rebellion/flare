# Integrations and monitoring

This page is the operator map for the integrations that ship with Flare. It
describes how to connect each one, which signals Flare records today, and what
still needs an external check or a planned metric. It describes the current
service behavior; `configured` means that credentials are stored, not that a
provider is reachable.

## Start with the service probes

Use these probes from the same network path as the load balancer or service
monitor:

| Endpoint | Auth | Meaning |
| --- | --- | --- |
| `GET /livez` | None | Process and HTTP listener are alive. It does not query Postgres or providers. |
| `GET /readyz` | None | Returns database reachability, database latency, analytics, retention, partition maintenance, email, and worker-pool state. Postgres failure returns `503`; a runtime component can be `degraded` while the HTTP status remains `200`. |
| `GET /api/health` | None | Compatibility alias for `/readyz`. |
| `GET /metrics` | Keep on a private network | Prometheus text with bounded HTTP request counters and latency histograms, plus selected outbound integration call counters and latency by fixed provider, operation, channel, and outcome labels. It contains no request parameters, credentials, tenant IDs, project IDs, or event payloads. |

`GET /metrics` is unauthenticated and is not an application API or OpenAPI
route. It **MUST** be bound to a private listener/network or protected by an
authenticated reverse-proxy rule; a proxy that exposes the whole service also
exposes these counters. The scrape is route-level and bounded, so it does not
include event payloads or item-level acceptance details. Durations are measured
inside Flare around handler or provider-method execution; they are not a
provider delivery receipt and do not include quota or rate-limit metadata.
The stable series are `flare_http_requests_total`,
`flare_http_request_duration_seconds`, `flare_integration_requests_total`, and
`flare_integration_request_duration_seconds`; labels stay limited to route,
method, status, provider, operation, channel, and outcome.

Do not use a `200` response or `configured: true` as proof that an outbound
provider accepted a request. The readiness body intentionally omits secrets and
provider error text. Build a provider-specific check from the signals below.

## Telemetry integrations

Create a project in the dashboard, then use its DSN public key with any
Sentry-compatible SDK or exporter. Ingest is DSN-authenticated and has a
per-DSN rate limit (`INGEST_RATE_PER_MIN`); it does not use a browser session.

| Integration | Send to | Stored and visible signals | Current alerts and limits |
| --- | --- | --- | --- |
| Sentry envelope and event | `POST /api/{projectID}/envelope`, `/store`, or `/events` | Error events are grouped into issues, with first/last seen, frequency, stack traces, release, and trace context. Transaction items become spans. | `new_issue`, `regression`, and `spike` rules run on issue evidence. The response can be partial; an accepted HTTP response is not a guarantee that every item was persisted, so inspect the response body and correlation ID. |
| OpenTelemetry HTTP | `POST /otlp/v1/logs`, `/otlp/v1/traces`, `/otlp/v1/metrics` (protobuf or JSON) | Logs, spans, and metrics are normalized into the same tenant-scoped store. OTLP responses report rejected items when validation or the item cap prevents full acceptance. | Logs, traces, and metrics are queryable from the project pages and MCP. There are no native severity, trace-percentile, or metric-threshold alert rules yet; use an exporter or scheduled query for those SLOs. |
| Native log and metric payloads | `POST /api/{projectID}/logs` and `/metrics` | Structured logs and metric samples are retained and can be searched or queried from the project pages. | Ingest errors, authentication failures, rate limits, and storage failures are returned to the sender and logged internally. The private `GET /metrics` endpoint exposes bounded service and selected integration counters; it does not expose telemetry contents. |
| Distributed tracing | Sentry transactions or OTLP spans | Trace waterfall, span attributes, trace ID, and latency analytics. SDK sampling is controlled by `FLARE_TRACES_SAMPLE_RATE` (default `0.1`; `0` disables). Flare avoids tracing its own ingest and health routes. | No trace-derived p95/p99 or SLO alert rule. Validate exporter acceptance using the ingest response; an SDK `Flush` only means its local queue drained. |
| Scheduled-job check-ins | `GET` or `POST /api/{projectID}/checkins/{slug}` with the project DSN; `?status=error` marks a failed run | A monitor is auto-created on first check-in. The dashboard shows interval, grace, last ping, last status, and state (`new`, `ok`, `failed`, `missing`). | Failed transitions page immediately; the watchdog pages overdue check-ins and re-notifies after 24 hours. A project can have at most 500 monitors. New slugs are capped and quiet, unconfigured monitors are pruned after 30 days. |
| Releases and source maps | Authenticated `POST /api/projects/{id}/releases` and `POST /api/projects/{id}/artifacts` | Release version and creation time, new-issue count, source-map artifact name and size, and symbolication for matching releases. | Release registry is capped at 2,000 versions per project. Source maps are capped at 30 MiB per upload, 2,000 artifacts, and 2 GiB per project. There are no release-health, missing-map, de-minification, or artifact-quota metrics; check upload responses and symbolication coverage. |

### Ingest checks for an exporter

1. Send one known event or OTLP batch with a request ID captured by the sender.
2. Treat `2xx` plus the response body as the acceptance result. For a partial
   response, retry only rejected items according to the response contract.
3. Confirm the issue, log, metric, or trace appears under the same project and
   tenant. A dashboard query proves persistence, while `/readyz` only proves
   database reachability.
4. Configure a check-in for every scheduled pipeline whose absence should page.

### Browser and frontend runtime capture

Flare's own Svelte dashboard has an optional dependency-free browser bridge in
`frontend/src/lib/flare-client.ts`. Set these **build-time** variables in
`frontend/.env` to enable it:

```dotenv
PUBLIC_FLARE_DSN=https://<public-key>@flare.example/123456789012
PUBLIC_FLARE_ENVIRONMENT=production
PUBLIC_FLARE_RELEASE=flare-web@<git-sha>
```

The bridge listens for `window` errors and unhandled promise rejections while
the app is being used, sends Sentry-compatible events to the project DSN, and
stops after a bounded per-minute budget. It removes query strings, fragments,
authorization-like values, and identifier-shaped path segments before sending.
It is disabled when `PUBLIC_FLARE_DSN` is empty. A DSN is an intentionally
public ingest credential; never put a member API key in browser configuration.

`bun run check` catches Svelte, TypeScript, and template syntax before a build;
the runtime bridge catches failures that only occur in a user's browser. A
cross-origin `Script error.` can still lack a useful stack when the browser
blocks source access. Upload a matching JavaScript source map for the same
release through the authenticated release/artifact API, and keep the upload
step in CI rather than serving maps publicly. Native Flutter symbol files are
covered separately below.

An app hosted on another origin needs an edge route or collector on that
origin, or an explicitly allow-listed CORS policy for the ingest endpoint.
Do not solve this with a wildcard credentialed CORS rule. Mobile and backend
clients do not have this browser constraint.

### Applying browser capture to first-party frontends

The same bridge is wired into the first-party browser bundles that ship in this
repository: Flare, Atomicsite, BrightCRM, Bright Community, Dockyard, Hash,
Sentinel, the SVAR portal, TrustIssues, Bright Hub, and the Astro marketing
site. SvelteKit and Astro builds read `PUBLIC_FLARE_DSN`,
`PUBLIC_FLARE_ENVIRONMENT`, and `PUBLIC_FLARE_RELEASE`; React/Vite builds read
the corresponding `VITE_FLARE_*` variables. The Dockerfiles take those values
as build arguments and the Compose files map them from `FLARE_DSN`,
`FLARE_ENVIRONMENT`, and `FLARE_RELEASE`.

The DSN is intentionally public and is safe to put in a static bundle. Keep
member/API keys and provider credentials out of all `PUBLIC_*` and `VITE_*`
variables. Empty DSNs leave the bridge disabled, which keeps local builds
quiet. A release value should be the exact commit or image release used for
the bundle; upload its matching JavaScript source map through the authenticated
release/artifact API so minified browser frames can be symbolicated. Build and
type checks catch syntax and compile errors before deploy, while the bridge
catches runtime errors, unhandled rejections, failed static resources, and
React render failures reported through `flare:exception`.
If an app sets a strict Content-Security-Policy, add `https://flare.example.com` (or the configured collector origin) to `connect-src`; otherwise the browser will block delivery before Flare can receive the event.

### Flutter applications

Use the starter in [`examples/flutter`](../examples/flutter/) for Android,
iOS, and Flutter web. It uses the Sentry-compatible `sentry_flutter` SDK with
the Flare DSN, and wires the framework, uncaught async, platform, and manual
handled-error paths into one project. Pass the DSN, environment, and release at
build time with `--dart-define`; keep `sendDefaultPii` off and scrub request or
user context in `beforeSend` before an event leaves the device.

The starter also shows navigation breadcrumbs, optional trace sampling, and a
bounded scheduled check-in for background work. Check-in silence detects a
dead or stopped worker; it does not prove that a mobile request reached your
backend. Use the backend integration below for server-side errors and traces,
and propagate a request or trace identifier when joining the two sides.

Flare currently symbolicates JavaScript source maps. Readable Dart VM/native
Android/iOS crash frames require a native symbol ingestion and upload path that
is not part of this repository yet; keep the original event, release, and
platform tags even when native symbolication is pending.

### Backend services

Use [`examples/backend`](../examples/backend/) as the small, language-neutral
contract for a Go, Node, Python, or other service. Each service should set a
stable `service.name`, `environment`, and `release`, send errors through a
Sentry-compatible SDK or OTLP/HTTP exporter, scrub secrets before export, and
emit a check-in for every scheduled worker whose silence should page. The
example includes a standard-library check-in command so a service can prove
the DSN and route before adding an SDK.

For an OpenTelemetry Collector, select the **OTLP/HTTP** exporter and point it
at `/otlp`; Flare does not currently expose the collector's default OTLP/gRPC
listener on port 4317. Inspect the OTLP response for rejected items and retain
the request ID; a local SDK `Flush` only drains its queue and is not a Flare
delivery receipt.

### CI and service heartbeats

CI producers can send ordinary Sentry or OTLP failures and a periodic
info-level `service-up:<service>` heartbeat. Heartbeats use a stable
`service-up` fingerprint, keep silence detection aware of a healthy quiet
service, and are excluded from anomaly/error-rate paging. A CI watchdog still
needs an expected cadence and a check-in or heartbeat for each producer;
Flare does not poll GitHub Actions or Hephaestus directly.

## Notification channels

Channels are configured per workspace from **Settings** or the channel API:

- `log` writes a structured alert to the service logger.
- `email` uses the configured SMTP mailer.
- `slack` posts to the configured Slack webhook.
- `webhook` posts JSON to a public HTTPS URL.

Outbound HTTP channels use SSRF validation, a 10-second timeout, and disabled
redirects. SMTP has bounded connection and send deadlines and enforces the
configured TLS mode. Channel records include `last_attempt_at`,
`last_completed_at`, `last_ok_at`, `last_error`, and a monotonic attempt
sequence. The **Send test** action exercises the real destination and updates
those fields. Dispatch fans out concurrently; a delivery failure is retained
per channel and an all-channel failure releases the alert claim for retry.

The workspace limits enabled channels to 25 (100 stored). The private
`/metrics` endpoint records bounded alert-channel attempts and latency by
channel and outcome. It does not expose provider quota or delivery receipts;
monitor the channel health fields and your mail provider separately.

## Outbound provider integrations

### GitHub issue linking

Configure an owner/repository and token in **Settings -> GitHub** or with
`PUT /api/integrations/github` (admin). The token is encrypted with
`FLARE_SECRET_KEY`; `GET /api/integrations/github` returns only `configured` and
`repo`. A member can create a GitHub issue from a Flare issue with
`POST /api/issues/{id}/github`.

Flare persists a delivery state machine (`claimed`, `sending`, `uncertain`,
`created`, `linked`) and reconciles an uncertain provider result by searching
for its marker before retrying. Global and per-workspace outbound limits protect
the provider. The durable delivery row keeps `last_error`, but the integration
read endpoint does not expose last success, latency, rate-limit, or token
validation state. The private `/metrics` endpoint records bounded create and
reconcile call counts and latency by operation and outcome. Add an
authenticated create/reconcile check when validating GitHub in production.

### OIDC single sign-on

An owner configures the issuer, client ID, secret, and default role in **Single
sign-on** or through the admin OIDC settings. Flare performs bounded discovery,
validates the returned issuer and endpoints, rejects unsafe redirects, caches
metadata, and binds the callback to the stored configuration. Start and callback
requests have IP, workspace, global, and in-process capacity limits.

`GET /api/integrations/oidc` reports configuration and the generated login and
redirect URLs. It does not persist provider probe results or expose login
success/failure counts, latency, or provider rate-limit headers. The private
`/metrics` endpoint records bounded discovery, JWKS key-fetch, token-exchange,
and userinfo invocation counts and latency by operation and outcome. Discovery
includes cache hits and waiters; JWKS is a network fetch. Exercise the full
start -> callback journey for an acceptance check; discovery at startup or
configuration time is not a continuous availability probe.

### BYOAI triage (OpenAI-compatible or Anthropic)

An admin configures the public HTTPS base URL, model, format (`openai` or
`anthropic`), API key, and optional auto-triage budget in **AI** settings or
`/api/integrations/ai`. Keys are encrypted with `FLARE_SECRET_KEY`. Flare
validates the endpoint against its SSRF policy, caps response bodies, scrubs
secrets and PII from evidence, and applies a 60-second client / 55-second
completion deadline. Private endpoints require the explicit
`FLARE_ALLOW_PRIVATE_AI_ENDPOINT` single-workspace exception.

Triage uses evidence fingerprints and a daily billable-call budget. A claim is
taken before a provider call and per-workspace completion concurrency is capped
at two. Provider failures map to safe `502`, `503`, or `429` responses with a
correlation ID. The config endpoint does not expose token usage, quota,
`Retry-After`, or a periodic provider probe. The private `/metrics` endpoint
records bounded AI call counts and latency by provider and outcome; use
provider billing/usage data for quota and spend.

### Error-to-fix workflow for an AI coding agent

Flare can triage an issue as soon as it is ingested. Enable **auto-triage** in
AI settings for bounded, background analysis of new fingerprints, or connect a
member-scoped API key to Claude Code, Cursor, or another MCP client. The agent
can call `overview`, `list_issues`, `get_issue`, `get_trace`, `search_logs`,
`query_metrics`, and `triage_issue`; the evidence is bounded and scrubbed before
it crosses the model boundary.

For an immediate coding loop, add a `new_issue` (and, where useful,
`regression`) webhook alert to a small worker. The worker should validate the
expected webhook shape, keep only the project and issue identifiers, then use
MCP to fetch the current evidence and run triage. It can open a draft GitHub
issue or prepare a branch/patch in the source repository with the stack, release,
reproduction hypothesis, tests to run, and the exact Flare issue link. Keep a
human approval gate before applying a patch, merging, or deploying. Flare does
not grant repository access to telemetry, execute instructions found in event
text, or deploy code on its own.

The webhook/MCP worker should record a receipt containing the issue ID, evidence
revision, triage result, source commit, and follow-up status. Retry with an
idempotency key, cap concurrency, and alert when triage fails or stays pending;
current auto-triage failures are logged and do not block ingest. Regressions on
an existing fingerprint and missing source maps still need an explicit agent
run or release check.

## Email and cold-tier storage

### SMTP

Set `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `SMTP_FROM`, and
`SMTP_TLS` (`starttls`, `tls`, or `none` for development). The mailer serves
alert channels, password resets, and invitations. `/readyz` reports only
`email: configured` or `email: disabled`; alert-channel sends add the detailed
channel attempt fields. The private `/metrics` endpoint records SMTP call
counts and latency by outcome. Password-reset and invitation sends do not have
a separate delivery receipt or periodic probe.

### Parquet, DuckDB, and object storage

Hot telemetry lives in Postgres. Analytics queries use DuckDB, and retention
exports aged partitions to the local `FLARE_PARQUET_DIR` or to S3-compatible
storage configured with `FLARE_PARQUET_S3_ENDPOINT`, `..._BUCKET`,
`..._ACCESS_KEY`, `..._SECRET_KEY`, `..._REGION`, and `..._USE_SSL`.

`/readyz` includes an analytics probe, retention policy (`export_before_drop`,
`paused`, or `drop_without_export`), and partition-manager last-run, success,
and error timestamps. By default, retention pauses rather than drops a
partition when export is unavailable. `FLARE_ALLOW_DROP_WITHOUT_EXPORT=true`
is an explicit opt-in to dropping without an archive. S3/MinIO connectivity,
object bytes, request latency, quota, and per-object erasure status are not
reported as first-class health metrics. Workspace/project deletion with a cold
object tier records a pending erasure for operator completion.

## Operator API, credentials, and security signals

### MCP

`POST /api/mcp` accepts a session or workspace API key and exposes bounded tools
for overview, projects, issues, logs, metric queries, traces, status changes,
and triage. It enforces a 120 request/minute and 120 work/minute workspace cap,
body, batch, and deadline limits, plus role checks for writes. MCP responses
scrub untrusted labels and provider data. The generic `/metrics` endpoint
includes MCP request status and latency by route, but there is no tool-level
latency, tool-error, authentication-failure, or usage metric yet.

### API keys and DSNs

API keys are role-scoped (`viewer` or `member`), tenant-scoped, and record
`last_used_at`; project DSNs are public ingest credentials. In-process limits
are deliberately fail-safe, but they are not coordinated by `REDIS_URL` across
replicas. Review key expiry and recent use from the keys page and rotate DSNs or
keys when a sender is decommissioned. There is no built-in unused-key or
near-expiry alert.

### Security and release signals

Rejected ingest authentication, key/project mismatches, and login lockouts are
recorded as issues in the `flare-security` project and can use `spike` and
`new_issue` rules. Sensitive payload detection scrubs responses and retained
evidence where applicable. A separate secret/PII-detected alert stream and
release-health rules (deploy failure, rollback, stale release, or first-seen
spike by release) are not implemented yet; add an external check if those are
required for an SLO.

## External app monitoring opportunities (not implemented)

The following apps were suggested as possible integration targets. None has a
native Flare collector in this repository. A Codex app/plugin connection also
does not connect that service to Flare. A future bridge would need explicit
credentials and scopes for each workspace, a bounded poller or webhook
receiver, and a project DSN to submit events, logs, metrics, or check-ins.
The exact signals available depend on the provider's granted permissions and
API; the table is a monitoring design map, not a promise of current access.

| Target | Useful signals for a future bridge |
| --- | --- |
| Dropbox | Connection and token status; file-change cursor age; create, update, delete, and share events; webhook lag; storage/quota and API errors. |
| Box | Connection and token status; enterprise/file events, sharing and permission changes, sync lag, storage/quota, and API errors. |
| Google Drive | Connection and token status; file and folder changes, sharing/permission drift, change-token age, storage/quota, and API errors. |
| SharePoint | Connection and token status; site/list/document changes, permissions, sync or webhook lag, storage/quota, and API errors. |
| Gmail | Connection and token status; mailbox watch freshness, new/failed or bounced mail where permitted, message-processing lag, quota, and API errors. |
| Outlook Email | Connection and token status; mailbox subscription freshness, inbound/outbound processing, failed or bounced mail where permitted, quota, and API errors. |
| Google Calendar | Connection and token status; event create/update/cancel flows, reminder or booking lag, conflicting bookings, watch freshness, quota, and API errors. |
| Outlook Calendar | Connection and token status; event and meeting changes, cancellation and booking lag, subscription freshness, quota, and API errors. |
| Linear | Connection and token status; issue/project state transitions, overdue work, automation failures, webhook lag, quota, and API errors. |
| Notion | Connection and token status; page/database changes, stale or failed sync, access changes, webhook/poll lag, quota, and API errors. |
| Figma | Connection and token status; file/version and comment activity where permitted, review handoff age, webhook lag, quota, and API errors. |
| Slack | Connection and token status; app membership and permissions, event or message delivery, alert acknowledgment lag, webhook failures, quota, and API errors. |
| Teams | Connection and token status; app membership and permissions, channel/message or meeting events where permitted, delivery lag, quota, and API errors. |
| Codex Security | Connection and scan availability; latest scan age and result, new or unresolved findings by severity, policy/CI failures, and API errors. |
| OpenAI Developers | Connection and project/key health; request volume, error and rate-limit rates, latency, usage and budget burn, quota, and API errors. |

For every bridge, measure authentication expiry, successful and failed calls,
`429`/quota responses, latency, retry backlog, last complete sync, source event
time versus ingest time, and a canary check-in. Keep raw message, document, and
design content out of health events unless the workspace explicitly authorizes
it. Permission failures and a silent event stream are separate conditions:
the first needs an error signal, while the second needs a heartbeat or a
known expected schedule to make a silence alert meaningful.

## Current alert surface and known limits

The built-in alert surface currently covers:

- issue `new_issue`, `regression`, and `spike` conditions;
- watchdog `anomaly` and `silence` conditions;
- failed and missing scheduled-job monitors; and
- delivery health for configured alert channels.

It does not yet provide first-class rules for log severity, metric thresholds,
trace latency percentiles, external provider health, release health,
object-store quotas, or MCP/API-key usage. The private `/metrics` endpoint
covers HTTP and selected outbound integration attempts; use provider billing,
object-store, and MCP/API-key data for the remaining signals. Export Flare logs
and readiness JSON to your infrastructure monitor, query the project APIs on a
schedule, and keep a check-in on the probe job itself. When these signals
become product requirements, add them at the dispatch and provider boundaries
so rejected, retried, timed-out, and accepted work can be distinguished.

The service metrics scrape also does not include item-level ingest outcomes,
background worker or partition transitions, database-pool state, MCP tool
usage, API-key/DSN usage, or S3 object operations. HTTP route counts only show
that a request reached a handler; they do not prove that every event, span,
metric sample, artifact, or provider-side message was accepted.
