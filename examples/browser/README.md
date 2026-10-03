# Flare browser bridge

`flare-browser.ts` is a dependency-free helper for first-party browser apps.
It contains two independent installers:

- `installFlareBrowserCapture` reports unhandled errors, rejected promises,
  failed static resources, and framework boundary events as Sentry-compatible
  events.
- `installFlareBrowserRum` reports allow-listed Web Vitals and API request
  health as native Flare metric points.

Both installers accept the public project DSN. A DSN is an ingest credential,
not a member/API key; it is safe to ship in a static bundle but remains
rate-limited and tenant-bound by Flare.

```ts
import {
  installFlareBrowserCapture,
  installFlareBrowserRum
} from './flare-browser';

const stopErrors = installFlareBrowserCapture({
  dsn: import.meta.env.PUBLIC_FLARE_DSN,
  environment: import.meta.env.PUBLIC_FLARE_ENVIRONMENT,
  release: import.meta.env.PUBLIC_FLARE_RELEASE
});
const stopRum = installFlareBrowserRum({
  dsn: import.meta.env.PUBLIC_FLARE_DSN,
  environment: import.meta.env.PUBLIC_FLARE_ENVIRONMENT,
  release: import.meta.env.PUBLIC_FLARE_RELEASE,
  sampleRate: 0.1,
  maxMetricsPerMinute: 20
});
```

RUM emits these stable names for `metric_threshold` rules:

| Name | Value |
| --- | --- |
| `browser.web_vital.lcp_ms` | Largest Contentful Paint milliseconds |
| `browser.web_vital.cls` | Cumulative Layout Shift score |
| `browser.web_vital.inp_ms` | Interaction to Next Paint milliseconds |
| `browser.api_request.latency_ms` | Same-origin `/api/` fetch duration |
| `browser.api_request.failure` | `1` for non-2xx or network failure |

The default request filter captures same-origin `/api/` fetches. Pass an
explicit `requestFilter` for another allow-listed API origin. Routes are
scrubbed to remove query strings, emails, and identifier-shaped path segments;
only method, status class, and the scrubbed route are retained. Request bodies,
headers, user identifiers, and response bodies are never sent. Sampling and a
per-minute point cap apply before transport. Web Vitals are flushed when the
page is hidden or unloaded.

The browser can only report that `sendBeacon` or `keepalive` queued a request;
that is not durable acceptance. For release verification, query the project's
ingest receipts and metric series after the app has emitted telemetry.

The small unit suite can be run without installing a browser SDK:

```sh
cd flare
bun test ./examples/browser/flare-browser.test.ts
```
