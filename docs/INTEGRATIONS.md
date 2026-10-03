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
| `GET /readyz` | None | Returns database reachability, latency and pool occupancy, optional analytics and retention state, email, bounded worker-pool occupancy, and queue mode. Postgres failure returns `503`; an optional runtime component can be `degraded` while the HTTP status remains `200`. |
| `GET /healthz` | None | Alias for `/readyz`, useful for platforms that reserve `healthz` as their probe name. |
| `GET /api/health` | None | Compatibility alias for `/readyz`. |
| `GET /metrics` | Keep on a private network | Prometheus text with bounded HTTP request counters and latency histograms, selected outbound integration call counters and latency, and process-wide ingest request/item outcome counters. Labels are fixed transport, signal, outcome, provider, operation, and channel values; the output contains no request parameters, credentials, tenant IDs, project IDs, or event payloads. |

`GET /metrics` is unauthenticated and is not an application API or OpenAPI
route. It **MUST** be bound to a private listener/network or protected by an
authenticated reverse-proxy rule; a proxy that exposes the whole service also
exposes these counters. The scrape is route-level and bounded, so it does not
include event payloads or tenant/project identifiers. Ingest outcomes are
aggregated by transport (`sentry`, `otlp`, `native`) and signal (`event`,
`transaction`, `log`, `span`, `metric`, `other`), with accepted, partial,
rejected, failed, and empty request outcomes and accepted, invalid, dropped,
and failed item counters. `flare_ingest_receipts_total` counts receipt write
errors by fixed operation and outcome labels. These process-wide counters are
not a tenant dashboard or a dump of receipt rows.
Durations are measured inside Flare around handler or provider-method
execution; they are not a provider delivery receipt and do not include quota or
rate-limit metadata. The stable series are `flare_http_requests_total`,
`flare_http_request_duration_seconds`, `flare_integration_requests_total`,
`flare_integration_request_duration_seconds`, `flare_ingest_requests_total`,
`flare_ingest_items_total`, and `flare_ingest_receipts_total`; labels stay
limited to route, method, status, transport, signal, outcome, provider,
operation, and channel. Durable receipt write failures are counted separately
with `flare_ai_action_receipts_total` and `flare_connector_receipts_total` so
the process metrics can expose a persistence problem without exposing row
contents.

The readiness JSON is also bounded and contains no tenant data. `db` reports
reachability, probe latency, and pool counts; `analytics` reports whether the
DuckDB layer opened and can read Postgres; `workers` reports only capacity,
busy, and available slots for the alert and auto-triage pools. `queue.mode` is
currently `bounded_drop`, with `queued: 0`, `durable: false`, and
`drop_on_full: true`: detached work is intentionally discarded when a bounded
pool is full, so operators can distinguish saturation from a durable backlog.

When the partition exporter is enabled, `analytics.export` in `/readyz` and
the authenticated `GET /api/analytics-health` endpoint expose the same global,
payload-free cold-tier ledger. The projection includes succeeded, failed, and
in-progress export counts, fixed failure-stage codes, export latency, latest
attempt/success/failure timestamps, and a byte total where the size is known.
Local Parquet files are stat'ed after the atomic rename, so their byte total is
exact. DuckDB's current `httpfs` COPY path does not return the remote object
length for S3/MinIO, so S3 bytes are explicitly reported with `bytes_known:
false`; Flare never guesses or lists object contents. A successful COPY is an
observed connectivity success and a failed COPY is an observed connectivity
failure, not a periodic provider ping. `connection_status` is one of
`disabled`, `unknown`, `in_progress`, `observed_success`, or
`observed_failure`. The ledger carries no tenant IDs, bucket names, URLs,
credentials, paths, provider responses, or telemetry payloads.

Authenticated viewers can query bounded durable outbound connector receipts with
`GET /api/connector-health?hours=24&limit=100`. The org-scoped response groups
alert, GitHub, AI, and OIDC attempts by fixed provider, operation, and channel
values and reports attempt counts, success/error counts and rate, elapsed-time
aggregates, and last-attempt timestamps. `hours` is clamped to 1--720 and
`limit` to 1--100. Receipts retain only organization provenance, optional
project provenance, outcome, HTTP status class or safe error code, duration,
and timestamps; URLs, response bodies, credentials, and tenant payloads are
never stored. Project alert rules can now use `type: "connector_error_rate"`
with a required `connector_provider`, optional `connector_operation` and
`connector_channel`, a `comparison` (`gt`, `gte`, `lt`, or `lte`), a decimal
`threshold_value` from `0` to `1`, and a `window_minutes` value from `1` to
`720`. Empty operation, channel, or error-code selectors match all values for
that provider; set `connector_error_code` to a safe token such as
`auth_expired` to page separately on credential failures. Rules do not fire
until at least one matching attempt exists. The watchdog aggregates only
project and organization scoped rows, so one tenant cannot observe or alert on
another tenant's connector activity.
For an app or service dashboard, use
`GET /api/projects/{id}/connector-health?hours=24&limit=100` to keep the same
aggregate scoped to one project. The project route verifies that the caller's
organization owns the project before reading connector rows.

Use `type: "connector_freshness"` when a provider series is expected to report
at a known cadence. It requires a provider, optional operation/channel
selectors, and a `window_minutes` cadence from `1` to `720`. The rule records
its own durable baseline when created, so an empty series pages only after the
configured cadence has elapsed; later it pages when the newest completed
attempt is older than that cadence. It never treats an unconfigured connector
or an arbitrary historical attempt as proof of an expected schedule.

Providers that expose rate-limit headers can include the optional bounded
`quota_remaining`, `retry_after_ms`, and `sync_lag_ms` integers on the same
connector attempt. `sync_lag_ms` is the age of the source watermark observed by
the bridge after a completed sync/read; Flare never infers lag from silence or
probes a provider.
These values are available through
`GET /api/connector-quota-health?hours=24&limit=100`, which returns the latest
value and observation time for each provider/operation/channel/installation
series. Registered installations include an opaque `connection_id`; legacy
external receipts remain grouped under an empty connection ID.
For an app or service dashboard, use
`GET /api/projects/{id}/connector-quota-health?hours=24&limit=100`; Flare
verifies that the caller's organization owns the project before returning the
same bounded projection.
`quota_remaining` is limited to `0..1000000000000000`; `retry_after_ms` and
`sync_lag_ms` are limited to `0..604800000` (seven days). A project alert with
`type: "connector_quota"` selects `connector_quota_metric` (`quota_remaining`,
`retry_after_ms`, or `sync_lag_ms`), the same optional selectors, a comparison
(`gt`, `gte`, `lt`, or `lte`), a bounded `threshold_value`, and a
`window_minutes` value from `1` to `720`. The watchdog evaluates the latest matching observation and
stays quiet until one exists; both the query and rule evaluation require the
project and organization scope.

Authenticated viewers can query durable, one-minute usage buckets with
`GET /api/usage?hours=24&limit=100`. The response is scoped to the caller's
organization and exposes only fixed `dsn`, `api_key`, and `mcp` surfaces,
allowlisted operations, opaque project or API-key row identifiers, counters,
and timestamps. It never returns credentials, request bodies, user IDs, or
provider payloads. API-key and MCP buckets are intentionally organization-wide
within the owning tenant (`project_id` is empty); DSN buckets retain project
scope. `hours` is clamped to 1--720 and `limit` to 1--100.

Project alert rules can use `type: "usage_threshold"` with a fixed
`usage_surface`, `usage_operation`, and `usage_measure` (`requests`, `errors`,
or `denied`), plus a `comparison`, `threshold`, and `window_minutes` from
`1` to `720`. A missing bucket series never fires an alert. `denied` counts
tenant-known authorization refusals: read-only API keys rejected by a REST
role gate and read-only MCP credentials refused a write tool. Invalid or
unknown credentials have no tenant identity and are deliberately not counted.
API-key and MCP usage alerts are organization-wide within the owning tenant;
the project rule supplies the notification channels and does not narrow those
two surfaces to another project's data.

After DSN authentication and body framing, each Sentry, OTLP, native, and
connector ingest request must start a durable `ingest_receipts` row before any
telemetry is persisted. It contains only the authenticated organization/project,
a sanitized or generated request ID, signal/protocol, counts, final HTTP status,
outcome, retryability, and timing; request bodies and credentials are never
stored. If the start write is unavailable, Flare returns `503` so the sender
can retry without an untracked acceptance. Completion writes use a short
detached deadline and remain diagnostic best-effort. The rows provide
request-level audit and reconciliation evidence; they are not per-event payload
copies.

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
| OpenTelemetry HTTP | `POST /otlp/v1/logs`, `/otlp/v1/traces`, `/otlp/v1/metrics` (protobuf or JSON) | Logs, spans, and metrics are normalized into the same tenant-scoped store. OTLP responses report rejected items when validation, an oversized metric-label document, or the item cap prevents full acceptance. Each metric point's raw labels are capped at 4 KiB; oversized points are rejected and reported through OTLP `partial_success` rather than truncated. | Logs, traces, and metrics are queryable from the project pages and MCP. Metric threshold rules can alert on a bounded series' min/max over a 1--720 minute window; trace percentile rules page on operation latency, and service-level rules page on sampled-span availability against a latency objective. Use a metric threshold on an explicit request-success or business series when availability must cover unsampled traffic. Keep metric labels low-cardinality and free of secrets or user data. |
| Native log and metric payloads | `POST /api/{projectID}/logs` and `/metrics` | Structured logs and metric samples are retained and can be searched or queried from the project pages. Native metric points whose raw labels exceed 4 KiB are rejected and the response's accepted count reflects that result; labels are never silently truncated. | Ingest errors, authentication failures, rate limits, and storage failures are returned to the sender and logged internally. The private `GET /metrics` endpoint exposes bounded service, ingest-outcome, and selected integration counters; it does not expose telemetry contents. Keep metric labels low-cardinality and free of secrets or user data. |
| Distributed tracing | Sentry transactions or OTLP spans | Trace waterfall, span attributes, trace ID, and latency analytics. SDK sampling is controlled by `FLARE_TRACES_SAMPLE_RATE` (default `0.1`; `0` disables). Flare avoids tracing its own ingest and health routes. For sampled backend HTTP requests, the transaction also records the bounded response status code and Sentry's standard HTTP status classification. The forwarding wrapper preserves `http.Flusher`, `http.Hijacker`, `http.Pusher`, and `io.ReaderFrom` when the underlying server supports them, so streaming and upgrades continue to work. | `GET /api/projects/{id}/analytics/span-latency?hours=24` returns p50/p95/p99; `trace_percentile` rules alert on one operation's p50/p75/p90/p95/p99 latency, and the service-level endpoint reports sampled-span availability and error-budget consumption. |
| Scheduled-job check-ins | `GET` or `POST /api/{projectID}/checkins/{slug}` with the project DSN; `?status=error` marks a failed run | A monitor is auto-created on first check-in. The dashboard shows interval, grace, last ping, last status, and state (`new`, `ok`, `failed`, `missing`). | Failed transitions page immediately; the watchdog pages overdue check-ins and re-notifies after 24 hours. A project can have at most 500 monitors. New slugs are capped and quiet, unconfigured monitors are pruned after 30 days. |
| External SaaS and app connectors | `POST /api/{projectID}/connector-attempts` with `X-Flare-Key` (or `sentry_key`) | A bridge in any web, Flutter, backend, or vendor app can submit one bounded provider/operation/channel result plus optional registered `connection_id`, `quota_remaining`, `retry_after_ms`, and observed `sync_lag_ms` integers. Flare validates the connection belongs to the authenticated workspace and matches the provider, then stores a payload-free connector attempt and ingest receipt. | `connector_error_rate` and `connector_quota` rules can page on a provider, registered connection, operation, channel, fixed error code (including `auth_expired`), quota, retry delay, or observed sync lag. The request accepts only safe bounded tokens, `ok`/`error`, an HTTP status class or safe error code, elapsed milliseconds, and bounded provider hints; response bodies, URLs, credentials, and user data are rejected by the contract. |
| Releases and source maps | Authenticated `POST /api/projects/{id}/releases` and `POST /api/projects/{id}/artifacts` | Release version and registry creation time, new-issue count, source-map artifact name and size, symbolication for matching releases, bounded release health via `GET /api/projects/{id}/release-health?hours=24&limit=100`, release freshness via `GET /api/projects/{id}/release-freshness?hours=24&limit=100`, and source-map coverage via `GET /api/projects/{id}/symbolication-health?hours=24&limit=100`. | Release registry is capped at 2,000 versions per project. Source maps are capped at 30 MiB per upload, 2,000 artifacts, and 2 GiB per project. Release, freshness, and symbolication health report aggregate metadata without raw payloads; `release_error_rate` can page on an explicit or latest recent release, while `release_freshness` requires an explicit deployed release and its canonical heartbeat. |
| Flutter/Dart and native mobile symbols | Authenticated `POST /api/projects/{id}/symbol-artifacts` with a member API key or session | Stores one bounded, release-scoped `dart_symbols`, `proguard`, `breakpad`, `dsym`, or `elf` artifact for Flutter, Android, or iOS. Flare records a SHA-256 digest, size, platform, format, upload receipt, and durable upload-time `validation_status`; list responses never return symbol bytes. | Artifact count is capped at 2,000 and native-symbol storage at 2 GiB per project, with 30 MiB per artifact. Upload acceptance proves durable retention; Flutter container validation is explicit, while format-specific frame rewriting remains pending until its resolver is enabled. |
| Client/runtime health | Authenticated `GET /api/projects/{id}/client-health?hours=24&limit=100` | Browser, Flutter, mobile, and backend SDK dimensions grouped by platform, environment, and release, with event volume, actionable errors and rate, affected issues, and first/last seen. | The hot-window query is bounded to 1--720 hours and 1--100 groups and returns no event bodies, stacktraces, labels, or tenant IDs. Emit a bounded web-vitals or request metric and use a `metric_threshold` rule when a client performance or availability threshold should page. |

### Ingest checks for an exporter

1. Send one known event or OTLP batch with a request ID captured by the sender.
2. Treat `2xx` plus the response body as the acceptance result. For a partial
   response, retry only rejected items according to the response contract.
3. Confirm the issue, log, metric, or trace appears under the same project and
   tenant. A dashboard query proves persistence, while `/readyz` only proves
   database reachability.
4. Configure a check-in for every scheduled pipeline whose absence should page.

An external app can report the result of a provider call without sending the
provider payload to Flare. The project DSN is the write credential; keep it in
the app's secret store for backend or mobile code and use a server-side proxy
for browser code when the provider result must remain private:

```sh
curl -X POST "https://flare.example/api/<project-id>/connector-attempts" \
  -H "X-Flare-Key: <project-public-key>" \
  -H "Content-Type: application/json" \
  -d '{"provider":"hubspot","connection_id":"<registered-connection-id>","operation":"contacts.upsert","channel":"http","outcome":"error","status_class":"5xx","error_code":"provider_error","elapsed_ms":842,"quota_remaining":4,"retry_after_ms":2500,"sync_lag_ms":18000}'
```

The response is `202` only after the connector attempt is stored. Flare derives
the attempt timestamps from its receive time and the bounded duration, so a
client cannot forge an old success or move a receipt into another project.

Flare's own outbound adapters (alerts, GitHub, OIDC, and AI) use the same
payload-free receipt shape. Their authenticated tenant/project scope is carried
through the connector context, and provider quota, retry-delay, or source-sync
hints are retained only when the adapter supplies bounded integers. This keeps
internal and external connector health on the same query and alert paths; a
missing hint stays unknown rather than being inferred from silence.

For request-level reconciliation, authenticated viewers can query
`GET /api/projects/{id}/ingest-receipts?limit=100`. The limit defaults to 100
and is clamped to 500; rows are newest first and include only the sanitized
request ID, signal/protocol, counts, outcome, HTTP status, retryability, and
timestamps. The route is scoped by the caller's organization and project ID,
and never returns raw request bodies or credentials. A receipt with
`retryable: true` records a receiver-side persistence failure; a partial or
rejected outcome records the counts the sender should reconcile with its own
exporter metrics.

An `in_progress` receipt older than the configured request deadline is an
acceptance uncertainty: the process may have exited after the start row was
written but before its completion row. Create a project alert with
`type: "ingest_acceptance"`, a `threshold` from `1` to `1000000`, and a
`window_minutes` from `1` to `720` to page on that bounded count. The watchdog
query returns only the tenant-scoped count of old incomplete receipts; it never
returns request IDs, bodies, credentials, or provider data. Treat the alert as
a retry/reconciliation signal and inspect the receipt list before replaying a
batch.

For release dashboards, authenticated viewers can query
`GET /api/projects/{id}/release-health?hours=24&limit=100`. `hours` is bounded
to 1--720 (default 24) and `limit` to 1--100 (default 100). Each row is
scoped by the caller's organization and project and contains the release
version, registry creation time, total events, actionable error events and rate,
distinct affected issues, and nullable first/last event times. A recent
release with no traffic is returned with zero counts so a deploy that never
reported telemetry is visible. The query does not select event bodies,
stacktraces, or source-map content.

For release freshness, authenticated viewers can query
`GET /api/projects/{id}/release-freshness?hours=24&limit=100`. The response
contains only explicitly marked deploys, their latest canonical heartbeat time,
and the computed age in seconds. A release is marked only by the authenticated
deploy-marker endpoint; an old event cannot make a release current. Runtime
heartbeats are info-level `service-up:<service>` events carrying the release.
The Go `flarereport` helper emits these automatically when its heartbeat is
enabled; Flutter, browser, and other backends must emit them explicitly. A
`release_freshness` watchdog rule requires `release_version` and pages when
that release has no heartbeat (including the initial post-deploy period) or its
last heartbeat is older than the configured 1--720 minute window. The query
and rule are scoped by both organization and project and never return event
bodies or heartbeat service names. If Flare cannot durably advance the marker,
the event and marker transaction is rolled back, the ingest response is
retryable, and the release remains stale until a later heartbeat is accepted.

When one release contains multiple independently running workers, use a
`service_freshness` watchdog rule instead. It requires the exact bounded
`service_name` from `service-up:<service>` plus the exact `release_version` and
pages when that one service has no heartbeat or its last heartbeat is older
than the configured 1--720 minute grace. The query matches the canonical
message, level, release, project, and organization together, so a healthy
frontend worker cannot mask a stopped backend worker that shares the release.
Only the latest heartbeat timestamp is aggregated; event bodies and payloads
never enter the alert or API response.

Flare initializes its own error reporting before configuration and database
startup. Release builds pass `FLARE_RELEASE` into the binary, so configuration,
migration, and database startup failures retain the exact release identity
before the HTTP server is listening. Shutdown drains the local event queue;
that drain still does not prove remote durable acceptance.

For source-map delivery and missing-map checks, authenticated viewers can query
`GET /api/projects/{id}/symbolication-health?hours=24&limit=100`. The same
1--720 hour and 1--100 row bounds apply. Each release row contains map count and
bytes, event and stacktrace-event counts, candidate frames (frames with a
filename and positive generated line), frames whose filename or basename has a
matching uploaded map, the unmapped count, a nullable coverage rate, and first
or last event times. Releases registered in the window are returned even when
they have no traffic. The query is scoped by both organization and project and
does not return source-map content, event payloads, or stack traces. Coverage
means artifact selection only. `GET /api/projects/{id}/artifacts` now exposes a
fixed `parse_status` for each map (`valid`, `invalid_json`, `invalid_mappings`,
`unsupported`, or `unvalidated` for rows retained before validation). Uploads
remain durable and return `201` for compatibility, while maps whose status is
not `valid` are excluded from read-time symbolication. The status contains no
parser message or map bytes, and the endpoint remains organization/project
scoped.

Native/mobile build pipelines can retain their debug artifacts in the same
release boundary with `POST /api/projects/{id}/symbol-artifacts`. The endpoint
accepts a bounded JSON body with `release`, `platform` (`flutter`, `android`, or
`ios`), `format` (`dart_symbols`, `proguard`, `breakpad`, `dsym`, or `elf`), a
basename `name`, and `content_base64`. Use a member API key in CI; the upload
is tenant-scoped, records an ingest receipt, and returns only metadata and a
server-computed SHA-256 digest. For example:

```bash
artifact_b64=$(base64 < build/symbols/app.android-arm64.symbols | tr -d '\n')
curl -fsS -X POST "https://flare.example/api/projects/$FLARE_PROJECT_ID/symbol-artifacts" \
  -H "Authorization: Bearer $FLARE_MEMBER_KEY" \
  -H 'Content-Type: application/json' \
  --data "$(jq -n --arg release "$FLARE_RELEASE" --arg content "$artifact_b64" \
    '{release:$release,platform:"flutter",format:"dart_symbols",name:"app.android-arm64.symbols",content_base64:$content}')"
```

List and delete operations expose only bounded metadata through
`GET /api/projects/{id}/symbol-artifacts` and
`DELETE /api/projects/{id}/symbol-artifacts/{artifactID}`. The upload response
and list rows include a durable `validation_status`. For Flutter
`dart_symbols`, Flare runs its bounded container check during upload and stores
`recognized`, `invalid`, or `unsupported`; Android/iOS formats without an
enabled parser remain `not_checked`. Artifacts created before this field was
added also remain `not_checked` until reuploaded. `GET /api/projects/{id}/native-symbol-health`
repeats this stored status for every row, so a health read does not inspect only one arbitrary artifact or load
symbol bytes. Flare does not mark frames readable merely because an upload
succeeded: Dart, ProGuard, Breakpad, dSYM, and ELF resolvers must validate each
format before native symbolication is reported. The endpoint keeps an explicit
`symbolication_status` of `retained_unresolved`; this status is also attached
to issue-event responses and included in AI triage evidence when a matching
release artifact exists. The current Flutter check recognizes executable
containers and DWARF section presence, but it does not rewrite frames. The
endpoint never returns symbol bytes. Keep the release value byte-for-byte
identical to the SDK event and retain the upload digest as CI evidence.

For release error-rate paging, create a project alert rule with
`type: "release_error_rate"`, a `threshold_value` between `0` and `1`, a
`comparison` of `gt`, `gte`, `lt`, or `lte`, and a `window_minutes` value from
`1` to `720`. Set `release_version` to a bounded version string (up to 128
bytes) to pin the rule to one release, or leave it empty to select the newest
release with a deploy marker or event in the watchdog window. The watchdog
selects and aggregates only the tenant/project release registry and event
counts; no event body, stacktrace, issue text, or source-map content enters
the alert. A release with no events stays quiet until it reports traffic.

For log-volume paging, create a project alert rule with `type: "log_severity"`,
`log_severity` set to one of `trace`, `debug`, `info`, `warn`, `error`, or
`fatal`, `threshold` set to the minimum record count, and `window_minutes`
from `1` to `720`. The API accepts `warning` and `critical` as aliases for
`warn` and `fatal`, then stores only the canonical value. The watchdog counts
only tenant- and project-scoped logs at that level; it never reads or includes
log bodies, attributes, trace IDs, or span IDs in the alert. A new rule stays
quiet until at least one matching log arrives and the configured count is
reached.

For provider quota and rate-limit paging, create a project alert rule with
`type: "connector_quota"`, a required `connector_provider`, an optional
`connector_connection_id` from the workspace's connector registry, optional
operation and channel selectors, and `connector_quota_metric` set to
`quota_remaining`, `retry_after_ms`, or `sync_lag_ms`. Use `comparison` plus a bounded
`threshold_value` (for example `lte 100` remaining calls or `gte 10000`
milliseconds retry delay, or `gte 900000` milliseconds of observed sync lag)
and a `window_minutes` value from `1` to `720`.
The rule compares the latest matching provider hint and does not fire before a
hint has been accepted. Query the same payload-free series with
`GET /api/connector-quota-health?hours=24&limit=100`. Connector error-rate and
freshness rules accept the same connection selector. Health rows include the
opaque `connection_id` when a registered installation produced the receipt;
legacy DSN rows leave it empty. A non-empty selector is tenant-checked and its
provider must match `connector_provider`, so two HubSpot installations cannot
silently share an alert series.

For client and runtime comparisons, authenticated viewers can query
`GET /api/projects/{id}/client-health?hours=24&limit=100`. The response groups
recent events by the SDK-reported `platform`, `environment`, and `release`, so a
web bundle, Flutter build, and backend service can be compared in one project.
`hours` is bounded to 1--720 (default 24) and `limit` to 1--100 (default 100).
Each row contains total volume, actionable error volume and rate, distinct
affected issues, and nullable first/last event times. Informational heartbeats
remain in total volume but are excluded from the error rate. This aggregate does
not include payloads, stacktraces, labels, URLs, or tenant IDs. The endpoint is
read-only; pair it with a bounded metric sent by the client (for example a web
vital or failed-request counter) and a `metric_threshold` alert rule when a
client SLO should notify an operator.

### Business and client KPI conventions

Business and client health use the same tenant-scoped metric store, so they do
not require a new connector or a second credential. Send one numeric sample per
project with a bounded name such as `business.orders.completed`,
`business.checkout.failures`, `client.web.lcp_ms`, or
`client.flutter.api_errors`. Keep names stable and put dimensions in the
bounded `labels` object when they need separate alerting. Metric threshold
rules accept exact `metric_labels` selectors for `environment`, `release`,
`route`, `method`, and `status_class`; the selector is tenant-scoped, so one
stable browser series can page separately for a production release or route
without creating a new metric name. Keep label values low-cardinality and
free of secrets or user identifiers.
Use `metric_threshold` for a bounded high/low value over 1--720 minutes and
query `GET /api/projects/{id}/metrics/query` for the same series when showing a
dashboard. This gives business and client KPIs the same tenant provenance,
retention, and alert delivery guarantees as infrastructure metrics.

### Service-level objectives

For sampled trace evidence, authenticated viewers can query
`GET /api/projects/{id}/analytics/service-level?hours=24&span_name=GET%20%2Fcheckout&latency_ms=750&target=0.999`.
The response contains span count, good/error/slow counts, availability, and
error-budget consumption. A span is good when it has no error status and meets
the latency objective. This is evidence over received spans; missing or
unsampled requests cannot establish request availability, so pair it with a
client or backend request counter when the SLO must cover unsampled traffic.

The endpoint bounds the window to 1--720 hours, latency to 24 hours, and span
operation names to 128 bytes. It returns aggregate values only. Use a
`trace_percentile` rule for operation latency paging, or a `service_level`
rule to page when the observed availability of one operation falls below its
target. Create a `service_level` rule with `span_name`, `threshold` set to the
good-span latency objective in milliseconds, `threshold_value` set to the
minimum availability target (for example `0.999`), and a 1--720 minute
`window_minutes`. A missing span series stays quiet; pair the rule with
`service_freshness` or a scheduled check-in when producer silence must page.

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

The same module exports `installFlareBrowserRum` for first-party real-user
monitoring. It sends native metric points to the DSN's
`/api/{project}/metrics` endpoint and uses these stable names, which can be
selected directly by a `metric_threshold` rule:

| Metric | Meaning |
| --- | --- |
| `browser.web_vital.lcp_ms` | Largest Contentful Paint in milliseconds |
| `browser.web_vital.cls` | Cumulative Layout Shift score |
| `browser.web_vital.inp_ms` | Interaction to Next Paint in milliseconds |
| `browser.api_request.latency_ms` | Same-origin `/api/` fetch latency |
| `browser.api_request.failure` | One point for a non-2xx or network failure |

The helper is dependency-free and safe to add to any browser bundle:

```ts
import { installFlareBrowserRum } from './flare-browser';

const stopRum = installFlareBrowserRum({
  dsn: import.meta.env.PUBLIC_FLARE_DSN,
  environment: import.meta.env.PUBLIC_FLARE_ENVIRONMENT,
  release: import.meta.env.PUBLIC_FLARE_RELEASE,
  sampleRate: 0.1,
  maxMetricsPerMinute: 20
});
```

Web Vitals are finalized when the page becomes hidden. Request capture wraps
`fetch`, defaults to same-origin `/api/` calls, and can be widened with an
explicit `requestFilter`. Metric names, values, labels, and timestamps are
allow-listed and bounded; labels keep only environment, release, method, HTTP
status class, and a scrubbed route with query strings, emails, and
identifier-shaped path segments removed. Request bodies, headers, user
identifiers, and provider responses are never sent. `sampleRate` is clamped to
`0..1` and `maxMetricsPerMinute` is
clamped to a bounded client-side budget, while the DSN's server-side ingest
limit remains the final control. `sendBeacon` and `keepalive` only confirm that
the browser queued a request; they do not prove durable acceptance. Use the
project's ingest-receipt endpoint and metric query after a release when an
operator needs acceptance evidence.

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

For first-frame startup, frame/jank, lifecycle, offline queue, and API health,
copy `examples/flutter/lib/flare_health.dart` and
`examples/flutter/lib/flare_flutter_health.dart`. The binding aggregates frame
timings and emits the existing native metric shape with stable names such as
`client.flutter.startup_ms`, `client.flutter.frame.max_ms`,
`client.flutter.jank_rate`, and `client.flutter.offline_queue_depth`. Its
`trackApi` wrapper emits `client.flutter.api_latency_ms.<operation>` plus the
existing payload-free `connector_attempts` shape (`provider: backend`,
`operation`, `channel`, `outcome`, safe status/error code, and elapsed time).
Pass the same build `environment` and `release` to `FlareHealthReporter`; it
copies bounded, selector-compatible values onto every native metric so a
`metric_threshold` rule can target one Flutter release without mixing builds.
Metric names and connector dimensions are reduced to bounded low-cardinality
tokens; query strings, identifiers, credentials, exception messages, response
bodies, and user data are not sent. The helper exposes `FlareDsn.parse` and
batch encoders, while the app owns its HTTP client, retry policy, and offline
queue. Queue depth should be reported after bounded changes, and a backend or
scheduler check-in remains the availability signal when the OS suspends or
terminates the mobile process. Run `flutter test test/flare_health_test.dart` from the copied helper's
application test suite.

Flare symbolicates JavaScript source maps and now retains bounded native symbol
artifacts for Flutter/Dart, Android, and iOS release pipelines. Native uploads
are durable artifact evidence; format-specific Dart, ProGuard, Breakpad, dSYM,
and ELF frame resolvers are still pending, so keep the original event, release,
and platform tags and do not claim native frames are readable until a resolver
has verified them.

### Backend services

Use [`examples/backend`](../examples/backend/) as the small, language-neutral
contract for a Go, Node, Python, or other service. Each service should set a
stable `service.name`, `environment`, and `release`, send errors through a
Sentry-compatible SDK or OTLP/HTTP exporter, scrub secrets before export, and
emit a check-in for every scheduled worker whose silence should page. The
example includes a standard-library check-in command so a service can prove
the DSN and route before adding an SDK.

The Atomicsite, BrightCRM, Sentinel, Svar, Mesh Cloud, Mesh Hub, and Reactor
backend binaries follow the same release contract as their browser bundles.
Their Docker builds accept
`FLARE_RELEASE` (as the service-specific Go build argument) and stamp it into
`main.Version`; Compose passes the same value to the frontend build. These
services initialize Flare before configuration and database startup. Atomicsite
captures configuration validation failures; BrightCRM, Sentinel, and Svar
capture terminal startup errors before exiting. Mesh Cloud now initializes
before opening its store and captures fatal store, server-construction, and
listen failures before flushing; Mesh Hub and Reactor initialize before their
dependency opens and capture their terminal startup paths. Svar's unified
binary keeps the same immutable release across its `svar-go`,
`scanner-dashboard-go`, and `svar-autoscaler` roles while retaining separate
Flare service names. All of these services drain the SDK's local queue on exit.
A local queue drain remains weaker than a Flare ingest receipt, so release
acceptance must still verify the corresponding durable receipt and event.

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
- `automation_webhook` posts a signed, payload-free alert envelope to an
  external coding or operations worker; the worker owns any later AI, CI,
  deploy, or synthetic actions.

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

### Connector connection registry

Flare keeps tenant-scoped connector installations in an admin-only registry.
Use `GET /api/connector-connections` to list the workspace's connections,
`GET /api/connector-connections/{id}` to read one, `POST
/api/connector-connections` to register one, `PATCH
/api/connector-connections/{id}` to rotate metadata or a credential, and
`DELETE /api/connector-connections/{id}` to disconnect it. The request accepts
`provider`, `installation_id`, an optional `installation_name`, `status`
(`pending`, `active`, `paused`, `error`, or `revoked`), a bounded `scopes`
array, and an optional `secret`.

The table is keyed by `(org_id, provider, installation_id)` and every query
binds the authenticated workspace. Secrets are encrypted with `FLARE_SECRET_KEY`
before storage. Responses contain only provider and installation metadata,
scopes, status, timestamps, and `credential_configured`; they never contain the
secret, its ciphertext, or the workspace ID. Omitting `secret` on a PATCH keeps
the encrypted credential unchanged; supplying a replacement re-encrypts it.
An `active` connection must have a credential. `POST
/api/connector-connections/{id}/check` supports four fixed-authority probes for
enabled connections: HubSpot account metadata (`account_details`), GitHub
`user`, Slack `auth.test`, and OpenAI `models`. It decrypts the credential only at the
outbound boundary, discards provider bodies, and returns only the outcome, HTTP
status class, and bounded quota or retry hints. Slack and OpenAI checks are
on-demand health checks; GitHub, Slack, and OpenAI checks do not persist or expose
account, workspace content, model records, or messages. The probe emits the same tenant-scoped connector attempt Flare uses
for its health dashboards and records the registered connection ID when the
call is made through the registry. It does not schedule polling or change the
administrator's chosen lifecycle status.

An administrator or external scheduler can run one bounded HubSpot worker pass
with `POST /api/connector-connections/{id}/sync`. The pass claims the
connection's short lease before reading the encrypted credential, calls the
same account-details adapter, emits a payload-free connector receipt, and
completes the lease with a fixed next-run time (15 minutes after success, five
minutes after ordinary failure, or a bounded provider retry delay). A busy
lease returns an accepted skipped result. This endpoint is an explicit worker
hook: Flare does not start a background polling ticker, and future record
syncs must use a separate operation and cursor contract.

Provider-agnostic worker state is retained in `connector_sync_states`. A worker
first creates a `(connection, operation)` state, atomically claims a short
lease, reads or writes only an encrypted cursor, and completes with a bounded
`ok`/`error` result, optional sync lag, and next-run time. Completion is
lease-owner guarded, so a late worker cannot overwrite a newer cursor. The
member-readable `GET /api/connector-sync-health` projection exposes state,
attempt count, cursor presence/version, lease status, timestamps, and fixed
error codes; it omits cursor material, credentials, provider payloads, and
workspace IDs. No provider scheduler is enabled by default. To run bounded
HubSpot account health passes inside Flare, set
`FLARE_CONNECTOR_SYNC_ENABLED=true` and an optional
`FLARE_CONNECTOR_SYNC_INTERVAL=5m` (the default cadence when omitted), then
restart the service. Each tick scans at most 20 due active installations, uses
a short lease and per-run deadline, and skips live leases; replicas can share
the scheduler without duplicate provider calls. Leave the flag off when an
external scheduler owns polling and call the explicit `/sync` hook instead.
Future record syncs must use a separate operation and cursor contract.

Create a project alert rule with `type: "connector_sync_health"` to page on
durable worker state for one registered installation. The rule requires the
tenant-checked `connector_connection_id`, the exact sync `connector_operation`
(for example `account_details`), and `connector_sync_mode` of `failed` or
`overdue`. `failed` counts recent terminal failures; `overdue` counts a missed
`next_run_at` or expired worker lease after the configured `window_minutes`
grace period. `threshold` is the number of matching states required to fire.
The alert remains project-routed, while the selected connection and sync state
are organization-owned; paused or revoked installations are excluded so an
intentional disconnect does not page. The watchdog returns only aggregate
counts and fixed selector labels, never cursors, credentials, or provider
payloads.

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

Each loaded issue triage also writes a payload-free `ai_action_receipts` row.
Authenticated viewers can query
`GET /api/projects/{id}/ai-receipts?limit=100`; the limit defaults to 100 and
is clamped to 500. Rows report the issue, operation, provider, status
(`started`, `succeeded`, `cached`, or `failed`), timing, retryability, and a
small safe error code such as `provider_error` or `not_configured`. Fresh
provider calls also carry a lowercase SHA-256 `evidence_digest` of the bounded,
scrubbed incident context placed inside the untrusted-data fence; cached reads
leave it empty. Prompts, responses, credentials, and upstream error text are
never stored or returned. The REST triage response and the `triage_issue` MCP
result include `evidence_digest` for a fresh completed call so a worker can
submit the exact digest as a passed patch receipt's `evidence_ref`; cached
results omit it.
The receipt start is part of triage acceptance: if Flare cannot write the
`started` row, triage returns a typed retryable `503` and does not call the
provider. Completion writes use a short detached deadline and remain
best-effort after the operation has been accepted.

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

Flare's receipt covers the triage action itself. The webhook/MCP worker should
use the returned fresh `evidence_digest` as the patch receipt's evidence
reference, then add its own receipt containing the issue ID, evidence revision,
source commit, and follow-up status so the coding and deployment steps remain
auditable.
Retry with an idempotency key, cap concurrency, and alert when triage fails or
stays pending; current auto-triage failures are logged and do not block ingest.
Regressions on an existing fingerprint and missing source maps still need an
explicit agent run or release check.

To page from Flare itself, create a project alert with `type: "ai_triage"`,
`ai_triage_mode: "failed"` or `"stuck"`, a `threshold` from `1` to `1000000`,
and a `window_minutes` value from `1` to `720`. `failed` counts terminal
failed triage receipts in the rolling window. `stuck` counts payload-free
`started` receipts that still have no completion after the configured age.
The watchdog aggregates only status, completion presence, and the presence of
a safe error code under the project and organization fence;
prompts, completions, issue content, credentials, and provider error text
never enter the alert reason.

Create a project alert with `type: "deploy_failure"`, a `threshold` from `1`
to `1000000`, and a `window_minutes` value from `1` to `720` to page when that
project records failed or blocked `deploy` receipts in the window. The watchdog
counts only the bounded receipt stage/status fields with the project and
organization predicates; receipt references and error codes are not included
in the alert reason. A missing series stays quiet. Rollback is not inferred
from a failed deploy: use the explicit `rollback` stage and configure a
rollback alert rule when rollback outcomes should page.

Create a project alert with `type: "deploy_acceptance"`, a `threshold` from
`1` to `1000000`, and a `window_minutes` value from `1` to `720` to page when
passed deploy receipts remain without a later passed synthetic receipt after
that grace period. The watchdog looks back only 24 hours and matches the same
organization, project, issue, and target revision. It returns an aggregate
count; receipt references, issue content, and probe evidence never enter the
alert reason. A synthetic pass must carry its bounded `evidence_ref`, so this
rule detects missing acceptance evidence rather than assuming a deploy is
healthy from a deploy receipt alone.

The worker can persist each bounded coding-loop stage with
`POST /api/projects/{id}/automation-receipts` using a member session or
member-scoped API key. The request names the issue, one of `triage`, `patch`,
`review`, `ci`, `merge`, `deploy`, `rollback`, or `synthetic`, a status (`pending`,
`approved`, `passed`, `failed`, or `blocked`), an idempotency key, and optional
evidence/source revision/target revision/run references. The `evidence_ref` on
a passed `patch` is required to be the 64-character lowercase SHA-256 digest
from a completed fresh AI triage receipt; other stage references are capped
and treated as opaque identifiers. Failed or blocked stages carry a small safe
`error_code`; retries set `retryable`. A duplicate idempotency key is accepted
only when its immutable fields match the original row.

For a passed `synthetic` stage, `evidence_ref` is required in addition to
`target_revision` and `run_ref`; use it for a bounded canary or probe result
identifier. Flare stores the reference as opaque audit evidence and never
fetches or executes it.

`GET /api/projects/{id}/automation-receipts?limit=100` is available to
authenticated viewers and is capped at 500 rows. An `approved` receipt must
come from a human session actor; the actor is taken from auth context rather
than the request body. This endpoint only records evidence. It never runs a
patch, merges a branch, deploys an artifact, or executes a synthetic check.
Prompts, model responses, source contents, credentials, and raw provider
payloads are never stored or returned.

An external coding-loop worker can poll
`GET /api/projects/{id}/automation-next?issue_id=...` after each receipt. The
read-only response is tenant- and project-scoped and contains only the issue,
target revision, eligible next stage, prerequisite receipt ID, and a bounded
state (`ready`, `pending`, `retry`, `blocked`, `complete`, or `not_started`).
`ready` is returned only after the immediately preceding stage is valid for the
same target revision; review approvals require a retained human actor, and a
synthetic pass requires both a runner and evidence reference. The endpoint
never checks out code, calls CI, deploys, or dereferences an evidence reference.
The worker performs that action externally, then posts its idempotent stage
receipt back to Flare.

Before running an executable stage, a worker should claim it with
`POST /api/projects/{id}/automation-claims`. The request names the issue, one of
`patch`, `ci`, `merge`, `deploy`, or `synthetic`, the target revision, a bounded
worker reference, and (for `patch`) the exact AI evidence digest. The lease is
between 1 and 3600 seconds (600 by default). The response contains an opaque
claim id and expiry; it is a handoff capability only and does not grant a
repository, CI, deployment, or probe capability. Claims are tenant/project/
issue/stage/revision scoped and the database atomically refuses a second active
claim. A passed executable receipt must include its claim id. Receipt insertion
checks the live lease and exact stage/revision/evidence binding in the same
transaction, consuming the claim on success. Failed or pending receipts may be
recorded without a claim for backward-compatible diagnostics and cannot advance
the workflow. Expired claims can be reclaimed; consumed claims cannot.

Completed workflow evidence is ordered by the target revision. A passed patch
must name `target_revision` and put the lowercase SHA-256 `evidence_digest`
from a completed, fresh `ai_action_receipts` triage row in `evidence_ref`.
Flare checks that digest with the same organization, project, and issue before
accepting the patch receipt; a cached triage row has no digest and cannot be
used as patch evidence. An approved review must name that same revision
and a human actor; then Flare accepts passed CI, merge, deploy, and synthetic
receipts only when the immediately preceding receipt exists for the same
issue, project, organization, and revision. CI, merge, deploy, and synthetic
passes also require a bounded `run_ref`. A passed synthetic receipt additionally
requires a bounded `evidence_ref` identifying the observed canary or probe
result; Flare treats that value as opaque and never dereferences it. This keeps
the final verification receipt tied to an external check result instead of only
to a runner identifier. Failed or blocked stages can be recorded without a
predecessor so the stopping point remains visible. A missing predecessor
returns a conflict instead of creating an ungrounded success receipt.

Rollback is a separate explicit stage. A rollback receipt must include both
`source_revision` (the revision being removed) and `target_revision` (the
revision restored); a passed rollback also requires `run_ref`. Configure a
`rollback` alert rule to page on explicit terminal rollback outcomes. Flare
never infers a rollback from an ordinary deploy failure or deploy success.

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
`paused`, or `drop_without_export`). The response also identifies the selected
cold tier (`local` or `s3`) and reports whether partition maintenance is
telemetry-backed. `analytics.export` and the authenticated
`GET /api/analytics-health` projection retain only bounded export counts,
latency, latest attempt/success/failure timestamps, and fixed failure-stage
codes. `retention_status` reports the configured export policy plus the latest
durable export outcome (`paused`, `drop_without_export`, `in_progress`,
`observed_success`, or `observed_failure`). It is an observed status and does
not claim that every aged partition has been archived. Local Parquet byte
totals are measured after the atomic rename. S3 and MinIO byte totals are
marked unknown because DuckDB `httpfs` does not return a remote object length
from the current COPY path. `quota_status` is therefore `not_configured` for
the local backend and `provider_unknown` for S3-compatible backends; exported
ledger bytes are never presented as current object-store occupancy. A
successful COPY is an observed cold-tier connectivity success; this is not a
periodic provider probe or a claim that every object is readable.
By default, retention pauses rather than drops a partition when export is
unavailable. `FLARE_ALLOW_DROP_WITHOUT_EXPORT=true` is an explicit opt-in to
dropping without an archive. Object-store quotas and per-object erasure status
remain operator/provider concerns; workspace/project deletion with a cold
object tier records a pending erasure for operator completion.

## Operator API, credentials, and security signals

### MCP

`POST /api/mcp` accepts a session or workspace API key and exposes bounded tools
for overview, projects, issues, logs, metric queries, traces, status changes,
and triage. It enforces a 120 request/minute and 120 work/minute workspace cap,
body, batch, and deadline limits, plus role checks for writes. MCP responses
scrub untrusted labels and provider data. The generic `/metrics` endpoint
includes MCP request status and latency by route. Authenticated viewers can
query `GET /api/usage?hours=24&limit=100` for durable, tenant-scoped MCP
request and error counters, grouped by the fixed protocol method or allowlisted
tool name. The response contains no JSON-RPC body, arguments, session ID, user
identifier, or model/provider data. Notifications and malformed requests that
never reach dispatch are not counted as tool calls; use the private route
metrics and security issue stream for those transport failures.

### API keys and DSNs

API keys are role-scoped (`viewer` or `member`), tenant-scoped, and record
`last_used_at`; project DSNs are public ingest credentials. Every successful
API-key authentication and DSN authentication increments a durable one-minute
usage bucket. API-key rows retain only the opaque key row ID as a credential
dimension; DSN rows retain only the project ID. Query the same bounded
`GET /api/usage` projection to review read/write/API-key/MCP volume and DSN
ingest volume by project. Error counts are recorded when MCP dispatch returns
an error; request-level DSN acceptance and rejection remains authoritative in
`GET /api/projects/{id}/ingest-receipts`. Unknown credentials cannot be
associated with a tenant and are therefore not written to usage rows.
In-process limits are deliberately fail-safe, but they are not coordinated by
`REDIS_URL` across replicas. Review key expiry and recent use from the keys
page and rotate DSNs or keys when a sender is decommissioned. Project alert
rules now include `api_key_hygiene`: choose `unused` to count org-owned keys
that have never been used or have been idle for the configured bounded horizon,
or `expiring` to count non-expired keys whose expiry is within that horizon.
The threshold is an aggregate key count and the horizon is in minutes (up to
90 days). The watchdog returns only the count; key names, prefixes, hashes,
expiry timestamps, and creator identities never enter the rule result.

### Security and release signals

Rejected ingest authentication, key/project mismatches, and login lockouts are
recorded as issues in the `flare-security` project and can use `spike` and
`new_issue` rules. Sensitive payload detection scrubs responses and retained
evidence where applicable. Release health and source-map coverage are queryable
from authenticated project endpoints. A bounded `release_error_rate` rule can
page on the newest recent release or an explicit release version. A
`deploy_failure` rule pages on failed or blocked deploy receipts. A separate
`rollback` rule pages on explicit terminal `rollback` receipts; ordinary deploy
failures and deploy successes never count as rollbacks. A `release_freshness`
rule pages on an explicitly deployed release whose canonical service-up
heartbeat is overdue. Map-parse outcomes and a separate secret/PII-detected
alert stream remain follow-ups; use the bounded coverage response with an
external check if those are required for an SLO today.

## External app monitoring opportunities (native collectors not implemented)

The following apps were suggested as possible integration targets. None has a
native Flare collector in this repository. The generic DSN connector-attempt
endpoint above is available now for a bridge to report bounded call outcomes;
provider-specific polling, webhooks, and change cursors still need explicit
credentials and scopes for each workspace. A Codex app/plugin connection also
does not connect that service to Flare by itself.
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
| GitHub | Connection and token status; repository/API permission health, issue or workflow delivery, webhook lag, quota, and API errors. |
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
- bounded `metric_threshold` rules over one metric series with optional exact low-cardinality label selectors;
- bounded `trace_percentile` rules over one span operation;
- bounded `service_level` rules over one span operation's observed availability and latency objective;
- bounded `connector_error_rate` rules over project-scoped connector attempts, optionally narrowed to one registered installation;
- bounded `connector_quota` rules over latest project-scoped quota, retry-after, or observed sync-lag hints, optionally narrowed to one registered installation;
- bounded `connector_freshness` rules over an explicitly configured connector cadence and optional installation selector;
- bounded `release_error_rate` rules over one explicit or latest recent release;
- bounded `release_freshness` rules over one explicitly deployed release and
  its canonical service-up heartbeat;
- bounded `service_freshness` rules over one exact service-up heartbeat and
  release dimension;
- bounded `deploy_failure` rules over failed or blocked deploy receipts;
- bounded `deploy_acceptance` rules over passed deploys that lack a later
  synthetic verification after the configured grace period;
- bounded `rollback` rules over explicit terminal rollback receipts;
- bounded `log_severity` rules over one canonical log level and rolling count;
- bounded `usage_threshold` rules over fixed DSN, API-key, or MCP operations;
- bounded `ai_triage` rules over failed or stuck payload-free AI receipts;
- bounded `ingest_acceptance` rules over old incomplete payload-free ingest
  receipts;
- failed and missing scheduled-job monitors; and
- delivery health for configured alert channels.

It does not yet provide a first-class object-store quota rule. Durable
MCP/API-key/DSN usage is queryable through
`GET /api/usage`, and the `usage_threshold` rule evaluates those counters with
tenant-safe fixed dimensions. The `deploy_failure` rule evaluates failed or
blocked deploy receipts. The private `/metrics` endpoint
covers HTTP and selected outbound integration attempts; use provider billing,
object-store, and usage API data for the remaining signals. Export Flare logs
and readiness JSON to your infrastructure monitor, query the project APIs on a
schedule, and keep a check-in on the probe job itself. When these signals
become product requirements, add them at the dispatch and provider boundaries
so rejected, retried, timed-out, and accepted work can be distinguished.

The service metrics scrape does not include durable receipt rows, background
worker or partition transitions, database-pool state, or S3 object operations.
Durable usage rows now cover successful MCP dispatch, API-key authentication,
and DSN authentication; they are intentionally separate from the process
scrape and from telemetry payloads. HTTP route counts only show
that a request reached a handler; they do not prove that every event, span,
metric sample, artifact, or provider-side message was accepted.
