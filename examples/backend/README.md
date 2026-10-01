# Backend integration kit

This directory contains copyable wiring for backend services, workers, and
scheduled jobs that send telemetry to Flare. It does not add an SDK dependency
to Flare itself or assume a particular application language.

The examples use Flare's current wire contracts:

| Workload | Recommended path | Credential |
| --- | --- | --- |
| Go, Node, Python, or another Sentry SDK backend | Sentry envelope through the project DSN | The DSN's public key is resolved by the SDK |
| OpenTelemetry SDK or Collector | OTLP/HTTP at `<flare-base>/otlp` | `X-Flare-Key: <project-public-key>` |
| Cron, CI, migration, or queue worker heartbeat | `GET` or `POST /api/{dsn-id}/checkins/{slug}` | The project DSN public key (`sentry_key` query parameter or `X-Flare-Key`) |
| Small custom sender | Native `/api/{dsn-id}/logs`, `/api/{dsn-id}/metrics`, or `/api/{dsn-id}/events` | The project DSN public key |

The public key is a write credential with a rate limit. It is intentionally
usable by SDKs, but it must still be scoped to one project, kept out of source
control, and rotated when a sender is retired. Never put a Flare API key or a
provider secret in an event.

## 1. Set one release and environment contract

Use the same values for errors, logs, traces, and metrics emitted by a service:

```sh
FLARE_DSN='https://<project-public-key>@flare.example.com/<numeric-dsn-id>'
FLARE_ENV='production'
FLARE_RELEASE='billing-api@<immutable-git-sha>'
FLARE_SERVICE='billing-api'
```

The DSN is the complete value copied from the Flare project. Its path is the
numeric DSN ID expected by Sentry-compatible SDKs. `FLARE_RELEASE` should be an
immutable build or commit identifier, not `latest`; `FLARE_ENV` should remain a
small stable set such as `production`, `staging`, or `development`. Flare
registers a release when an event reports it, and also exposes authenticated
release/artifact APIs for deploy tooling.

Do not put request bodies, cookies, authorization headers, access tokens, or
unredacted user identifiers into telemetry. Scrub in the application before the
event crosses the network, even though Flare sanitizes fields again at ingest.

## 2. Go with `sentry-go`

Pin `github.com/getsentry/sentry-go` in the application that is being
instrumented. The following is a complete initialization shape; keep the
scrubber owned by the application so it knows which fields are sensitive:

```go
package telemetry

import (
	"os"
	"strings"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

func InitFlare() (func(), error) {
	dsn := os.Getenv("FLARE_DSN")
	if dsn == "" {
		return func() {}, nil // local runs can intentionally disable reporting
	}
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Release:          os.Getenv("FLARE_RELEASE"),
		Environment:      os.Getenv("FLARE_ENV"),
		AttachStacktrace:  true,
		EnableTracing:    true,
		TracesSampleRate: 0.1,
		SendDefaultPII:   false,
		// Flare's pinned sentry-go v0.47 path uses the legacy transport here.
		// Mesh recorded a telemetry-buffer scheduler handoff risk on process
		// exit; keep this until a real exit-path delivery check passes.
		DisableTelemetryBuffer: true,
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			scrub(event)
			return event
		},
		BeforeSendTransaction: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			scrub(event)
			return event
		},
	}); err != nil {
		return nil, err
	}
	return func() { sentry.Flush(2 * time.Second) }, nil
}

func scrub(event *sentry.Event) {
	// Keep exception type, stack, release, environment, and trace IDs useful.
	event.User = sentry.User{}
	if event.Request == nil {
		return
	}
	event.Request.Data = ""
	event.Request.QueryString = ""
	event.Request.Cookies = ""
	for key := range event.Request.Headers {
		if strings.EqualFold(key, "authorization") ||
			strings.EqualFold(key, "cookie") ||
			strings.EqualFold(key, "set-cookie") {
			delete(event.Request.Headers, key)
		}
	}
}
```

Call the returned flush function during graceful shutdown. `sentry.Flush` only
waits for the SDK's local queue; it is not a durable delivery receipt. The
`DisableTelemetryBuffer` setting is deliberately tied to the sentry-go v0.47
shutdown behavior above; if you remove it while upgrading the SDK, run a real
collector-backed acceptance test that starts a short-lived process, captures a
known event, flushes, exits, and verifies that Flare stored the event. A unit
test or an SDK `Flush` return value alone does not prove that exit-path delivery
completed. For critical jobs, keep a bounded OpenTelemetry Collector or another
durable queue between the worker and Flare.

Attach the application request ID to handled errors so an operator can join the
application log, the Flare event, and the failed request:

```go
sentry.WithScope(func(scope *sentry.Scope) {
	scope.SetTag("request_id", requestID)
	scope.SetTag("service", os.Getenv("FLARE_SERVICE"))
	sentry.CaptureException(err)
})
```

Use a route template (for example, `/customers/{id}`) instead of a raw URL
containing IDs or query parameters. If an ingest response is a failure, retain
its `X-Request-ID` and `X-Flare-Error-ID` in the sender's structured log; do not
log the DSN URL.

## 3. Node.js with `@sentry/node`

Pin `@sentry/node` in the instrumented service and initialize it before the
HTTP framework is loaded:

```js
import * as Sentry from '@sentry/node';

Sentry.init({
  dsn: process.env.FLARE_DSN,
  environment: process.env.FLARE_ENV,
  release: process.env.FLARE_RELEASE,
  tracesSampleRate: 0.1,
  sendDefaultPii: false,
  beforeSend(event) {
    event.user = undefined;
    if (event.request) {
      event.request.data = undefined;
      event.request.query_string = undefined;
      event.request.cookies = undefined;
      if (event.request.headers) {
        for (const key of Object.keys(event.request.headers)) {
          if (/^(authorization|cookie|set-cookie)$/i.test(key)) {
            delete event.request.headers[key];
          }
        }
      }
    }
    return event;
  },
});

export function reportHandled(error, requestId) {
  Sentry.withScope(scope => {
    if (requestId) scope.setTag('request_id', requestId);
    scope.setTag('service', process.env.FLARE_SERVICE ?? 'unknown');
    Sentry.captureException(error);
  });
}

// Await this in the shutdown path. It drains the local SDK queue only.
export async function flushTelemetry() {
  await Sentry.flush(2000);
}
```

Install the framework's Sentry error handler according to the pinned SDK
version, after application routes are registered. Keep the framework's normal
error response and access-log correlation; an SDK capture is not a client
response acknowledgement.

## 4. Python with `sentry-sdk`

The callback runs before an event leaves the process. Flask, Django, and ASGI
integrations can be enabled through the pinned `sentry-sdk` extras, but the
same initialization contract applies:

```python
import os
import sentry_sdk


def scrub(event, hint):
    event.pop("user", None)
    request = event.get("request")
    if request:
        request.pop("data", None)
        request.pop("query_string", None)
        request.pop("cookies", None)
        headers = request.get("headers") or {}
        for key in list(headers):
            if key.lower() in {"authorization", "cookie", "set-cookie"}:
                headers.pop(key, None)
    return event


sentry_sdk.init(
    dsn=os.environ.get("FLARE_DSN"),
    environment=os.environ.get("FLARE_ENV"),
    release=os.environ.get("FLARE_RELEASE"),
    traces_sample_rate=0.1,
    send_default_pii=False,
    before_send=scrub,
)


def report_handled(error, request_id):
    with sentry_sdk.push_scope() as scope:
        if request_id:
            scope.set_tag("request_id", request_id)
        scope.set_tag("service", os.environ.get("FLARE_SERVICE", "unknown"))
        sentry_sdk.capture_exception(error)
```

Flush the SDK during a graceful worker shutdown if the pinned integration
exposes a flush method. A process termination or forced container kill can
discard a queued event; use a Collector for delivery that must survive a worker
restart.

## 5. OpenTelemetry over HTTP

Flare accepts OTLP/HTTP at the base `<flare-base>/otlp` and appends the standard
`/v1/logs`, `/v1/traces`, and `/v1/metrics` paths. The DSN public key is sent as
`X-Flare-Key`; it is not a session/API key. For SDKs that support the standard
environment variables:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT='https://flare.example.com/otlp'
OTEL_EXPORTER_OTLP_HEADERS='X-Flare-Key=<project-public-key>'
OTEL_SERVICE_NAME='billing-api'
OTEL_RESOURCE_ATTRIBUTES='deployment.environment.name=production,service.version=<immutable-sha>'
OTEL_TRACES_EXPORTER='otlp'
OTEL_LOGS_EXPORTER='otlp'
OTEL_METRICS_EXPORTER='otlp'
```

`examples/backend/otel-collector.yaml` is a small Collector bridge with a
batch processor and an explicit attribute redaction processor. Replace the
example host and extend the deletion list for the application's schema. Keep
the Collector's endpoint private; only its outbound connection needs to reach
Flare.

Use the same W3C trace context across service boundaries. Put `trace_id` and
`span_id` in logs through the SDK/Collector rather than copying an entire
request payload into an event. Flare stores OTLP logs, spans, and metrics in the
same project, but its response can contain `partial_success` when records are
invalid or a request cap is reached. Inspect that response and export failure
metrics from the sender.

The private `GET /metrics` route is Flare's Prometheus-style service scrape. It
is not the OTLP metrics ingest route and must not be exposed through a public
application proxy.

## 6. Scheduled jobs and check-ins

Configure a monitor in the Flare project and put its generated URL in a
protected job variable:

```sh
export FLARE_CHECKIN_URL='https://flare.example.com/api/<numeric-dsn-id>/checkins/nightly-backup?sentry_key=<project-public-key>'
examples/backend/checkin.sh        # successful run
examples/backend/checkin.sh error   # failed run
```

The URL writes monitor state, so both `GET` and `POST` are accepted. The slug is
bounded and the project has a monitor cap; use one stable slug per job instead
of embedding run IDs. An `ok` check-in records the last ping. `status=error`
marks the run failed and can page immediately; silence past the configured
interval and grace period is handled by the watchdog. A check-in proves that a
job reached Flare, not that its downstream work produced the expected business
result.

Do not print `FLARE_CHECKIN_URL`: it contains the same public ingest key used by
the project DSN. The script has bounded retries for transient transport/HTTP
failures. If the final request outcome is unknown, inspect the monitor in Flare
before manually rerunning a side-effecting job.

## 7. Delivery and verification contract

Treat every sender as at-least-once and every acknowledgement as bounded:

1. **Sentry envelope/event:** a `2xx` response can include `status: "partial"`
   with accepted, failed, invalid, dropped, or dropped-span counts. A `503`
   means the sender should retry with bounded backoff. A malformed `4xx` should
   be fixed or dropped rather than retried forever.
2. **OTLP:** inspect the protocol response's `partial_success` fields. Retry
   `429`, `5xx`, and transport failures with a bounded backoff and preserve the
   request ID from response headers. Do not replay an unbounded batch.
3. **Native JSON logs/metrics:** inspect the `accepted` count. These endpoints
   do not provide an SDK queue or an idempotency key; the caller owns retry and
   duplicate policy.
4. **Check-ins:** use one stable slug and a bounded retry. The endpoint has no
   caller-supplied idempotency token; an unknown result should be checked in the
   monitor UI/API before rerunning work with external side effects.

For a staging acceptance check:

1. Emit one known error marker with the release and environment above; verify
   the issue and event in the intended project.
2. Export one trace and one log carrying the same trace ID; verify both are
   queryable and linked.
3. Run the check-in script with `ok`, then with `error`, and confirm the monitor
   state and alert behavior.
4. Send a deliberately oversized or redacted test payload only in staging and
   verify the sender records the partial/rejected response without logging raw
   secrets.
5. Stop the scheduled worker and wait through its configured grace period to
   verify a missing check-in page. Keep a check-in on the probe job itself.

The examples do not prove provider delivery, database health, release artifact
symbolication, or business-level job correctness. Use Flare's `/readyz`, the
project pages, exporter logs, and the sender's own provider/quota telemetry as
separate acceptance signals.
