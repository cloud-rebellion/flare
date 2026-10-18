<script lang="ts">
  // Public docs. No auth needed.
  const sentrySnippet = `import * as Sentry from '@sentry/node';

Sentry.init({
  dsn: 'https://<key>@your-flare-host/<project-id>',  // from your project page
  release: '1.4.2',
  environment: 'production'
});

// errors are captured automatically; or:
Sentry.captureException(new Error('payment gateway timeout'));`;

  const curlSnippet = `curl -X POST "https://your-flare-host/api/<project-id>/store/?sentry_key=<key>" \\
  -H 'Content-Type: application/json' \\
  -d '{
    "level": "error",
    "platform": "node",
    "release": "1.4.2",
    "exception": { "values": [{ "type": "Error", "value": "boom" }] }
  }'`;

  const otlpSnippet = `# Point any OpenTelemetry exporter at Flare:
OTEL_EXPORTER_OTLP_ENDPOINT=https://your-flare-host/otlp
OTEL_EXPORTER_OTLP_HEADERS=x-flare-key=<key>`;

  const flutterSnippet = `import 'package:sentry_flutter/sentry_flutter.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await SentryFlutter.init((options) {
    options.dsn = const String.fromEnvironment('FLARE_SENTRY_DSN');
    options.environment = const String.fromEnvironment('FLARE_ENV', defaultValue: 'production');
    options.release = const String.fromEnvironment('FLARE_RELEASE');
    options.sendDefaultPii = false;
    options.tracesSampleRate = 0.1;
  }, appRunner: () => runApp(const MyApp()));
}`;

  const frontendSnippet = `# Optional Flare dashboard browser capture (build-time)
PUBLIC_FLARE_DSN=https://<public-key>@your-flare-host/<project-id>
PUBLIC_FLARE_ENVIRONMENT=production
PUBLIC_FLARE_RELEASE=flare-web@<git-sha>

# Compile and template checks still run in CI:
bun run check
bun run build`;

  const checkinSnippet = `# Keep a scheduled backend job's silence distinguishable from success
FLARE_CHECKIN_URL='https://your-flare-host/api/<project-id>/checkins/nightly?sentry_key=<key>'
curl --fail --connect-timeout 5 --max-time 20 "$FLARE_CHECKIN_URL"
# Add ?status=error on a failed run.`;

  const sourcemapSnippet = `# In CI, after your build, upload each .map for the release:
curl -X POST "https://your-flare-host/api/<project-id>/artifacts" \\
  -H "Authorization: Bearer $FLARE_API_KEY" \\
  -H 'Content-Type: application/json' \\
  -d "$(jq -n --arg c "$(cat dist/app.min.js.map)" \\
        '{release:"1.4.2", name:"app.min.js", content:$c}')"`;

  const releaseSnippet = `# Mark a deploy from CI:
curl -X POST "https://your-flare-host/api/<project-id>/releases" \\
  -H "Authorization: Bearer $FLARE_API_KEY" \\
  -H 'Content-Type: application/json' \\
  -d '{"version": "1.4.2"}'`;

  const mcpSnippet = `[mcp_servers.flare]
url = "https://your-flare-host/api/mcp"
bearer_token_env_var = "FLARE_MCP_TOKEN"
startup_timeout_sec = 20
tool_timeout_sec = 60`;
</script>

<article class="prose-flare max-w-none">
  <h1 class="text-2xl font-semibold tracking-tight">Flare docs</h1>
  <p class="mt-2 text-sm leading-relaxed text-zinc-400">
    Flare is sovereign, self-hostable observability: errors, logs, traces and metrics on one Postgres. Send data with any
    Sentry SDK (change only the DSN) or over OpenTelemetry. Use the dashboard for interactive administration and scoped org API keys for automation.
  </p>

  <div class="mt-6 grid grid-cols-1 gap-3 sm:grid-cols-3">
    {#each [['Errors', 'Grouped issues, symbolicated stack traces, regression + spike alerts'], ['Logs', 'Structured search + volume, DuckDB-backed'], ['Traces', 'Span waterfalls + latency percentiles']] as [h, d] (h)}
      <div class="rounded-lg border border-zinc-800/80 bg-zinc-900/40 p-4">
        <div class="text-sm font-medium text-zinc-200">{h}</div>
        <div class="mt-1 text-xs leading-relaxed text-zinc-500">{d}</div>
      </div>
    {/each}
  </div>

  <h2 id="send" class="mt-10 scroll-mt-20 text-lg font-semibold tracking-tight">Send your first event</h2>
  <p class="mt-1 text-sm text-zinc-400">
    Create a project in the dashboard to get a <strong>DSN</strong>. Then pick a path:
  </p>

  <h3 class="mt-5 text-sm font-medium text-zinc-300">A Sentry SDK (recommended)</h3>
  <p class="mt-1 text-sm text-zinc-500">Any <code class="font-mono text-zinc-400">@sentry/*</code> SDK works. Change only the DSN host.</p>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{sentrySnippet}</pre>

  <h3 class="mt-5 text-sm font-medium text-zinc-300">Or plain HTTP</h3>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{curlSnippet}</pre>

  <h3 class="mt-5 text-sm font-medium text-zinc-300">Logs, traces & metrics over OpenTelemetry</h3>
  <p class="mt-1 text-sm text-zinc-500">Point any OTLP exporter at Flare; the OTLP endpoint is on your project page.</p>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{otlpSnippet}</pre>

  <h3 class="mt-5 text-sm font-medium text-zinc-300">Flutter apps</h3>
  <p class="mt-1 text-sm text-zinc-500">
    Add <code class="font-mono text-zinc-400">sentry_flutter</code> and pass the DSN with
    <code class="font-mono text-zinc-400">--dart-define</code>. The SDK captures framework, async, native, and handled
    errors while the app is running; keep PII disabled and scrub app-specific fields before sending.
  </p>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{flutterSnippet}</pre>

  <h3 class="mt-5 text-sm font-medium text-zinc-300">Frontend runtime and syntax checks</h3>
  <p class="mt-1 text-sm text-zinc-500">
    The Flare dashboard has an optional bounded browser bridge for runtime errors and unhandled promise rejections.
    Build-time type and template syntax checks remain separate from runtime capture.
  </p>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{frontendSnippet}</pre>

  <h3 class="mt-5 text-sm font-medium text-zinc-300">Scheduled workers</h3>
  <p class="mt-1 text-sm text-zinc-500">Use a check-in for every job where a missing run should page. A 2xx acknowledges the check-in request, so verify monitor state during rollout.</p>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{checkinSnippet}</pre>

  <h2 id="sourcemaps" class="mt-10 scroll-mt-20 text-lg font-semibold tracking-tight">Readable stack traces (source maps)</h2>
  <p class="mt-1 text-sm text-zinc-400">
    Upload a <code class="font-mono text-zinc-400">.map</code> per release and minified frames resolve to your original
    source, with context. Do it from CI with a <strong>member</strong> API key (create one in Settings; viewer keys are read-only):
  </p>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{sourcemapSnippet}</pre>
  <p class="mt-2 text-xs text-zinc-500">
    Uploads stay durable for CI compatibility and return a bounded parser outcome in the artifact list:
    <code class="font-mono text-zinc-400">valid</code>, <code class="font-mono text-zinc-400">invalid_json</code>,
    <code class="font-mono text-zinc-400">invalid_mappings</code>, <code class="font-mono text-zinc-400">unsupported</code>,
    or <code class="font-mono text-zinc-400">unvalidated</code>. Only valid maps are used for symbolication; map bytes and parser messages never appear in metadata.
  </p>

  <h2 id="releases" class="mt-10 scroll-mt-20 text-lg font-semibold tracking-tight">Track releases</h2>
  <p class="mt-1 text-sm text-zinc-400">
    Set <code class="font-mono text-zinc-400">release</code> on your events and Flare tracks it automatically. Mark a
    deploy from CI so it shows with its time, and every issue records the version it was first seen in:
  </p>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{releaseSnippet}</pre>
  <p class="mt-2 text-xs text-zinc-500">
    A deploy marker is distinct from an event-created release. The built-in
    runtime heartbeat (<code class="font-mono text-zinc-400">service-up:&lt;service&gt;</code>) carries the same release;
    query <code class="font-mono text-zinc-400">/api/projects/&lt;id&gt;/release-freshness</code> or add a
    <code class="font-mono text-zinc-400">release_freshness</code> alert to page when that heartbeat is overdue.
    For releases with multiple workers, use <code class="font-mono text-zinc-400">service_freshness</code> with the
    exact service name and release so one healthy worker cannot hide another stopped worker.
  </p>

  <h2 id="mcp" class="mt-10 scroll-mt-20 text-lg font-semibold tracking-tight">Connect an AI over HTTP MCP</h2>
  <p class="mt-1 text-sm leading-relaxed text-zinc-400">
    Open <a href="/settings#mcp" class="text-amber-400 hover:text-amber-300">Settings → Connect your AI to Flare</a>.
    A workspace administrator can create a dedicated MCP token, choose read-only or read and triage access,
    set its expiry, and test it. Tokens cover the workspace's projects and expire within 90 days.
    They cannot authenticate to the management REST API.
  </p>
  <p class="mt-3 text-sm leading-relaxed text-zinc-500">
    For Codex, keep the token in the environment that launches the client and reference its variable name below.
    Other clients can use the same HTTPS endpoint with Streamable HTTP and a Bearer credential in their secret storage.
    Keep token values out of configuration examples, repositories, browser bundles, and chat.
  </p>
  <pre class="mt-3 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px] leading-relaxed text-zinc-300">{mcpSnippet}</pre>
  <p class="mt-2 text-xs leading-relaxed text-zinc-500">
    After connecting, call <code class="font-mono text-zinc-400">get_capabilities</code> to check the resolved workspace,
    role, and allowed tools. Start investigating with <code class="font-mono text-zinc-400">overview</code>.
    An existing client process may need to reconnect from the correct environment. This flow uses workspace tokens;
    browser OAuth authorization is not available here.
  </p>

  <h2 class="mt-10 text-lg font-semibold tracking-tight">Everything else</h2>
  <p class="mt-1 text-sm text-zinc-400">
    Issues, alerts (Slack/email/webhook/automation webhook), GitHub issue creation, team roles, SSO and more are in the
    <a href="/docs/api" class="text-amber-400 hover:text-amber-300">API reference</a>. Self-hosting? See the
    <a href="https://github.com/bright-interaction/flare/tree/main/deploy" class="text-amber-400 hover:text-amber-300">deploy guide</a>
    (Docker Compose + Helm).
  </p>
</article>
