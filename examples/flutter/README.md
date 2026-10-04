# Flutter client integration

This example connects a Flutter application to a Flare project through the
Sentry-compatible envelope API. It is intentionally a small copyable helper,
not a second Flutter package or a fork of the Sentry SDK.

## What is captured while the app is running

`sentry_flutter` captures these signals after `SentryFlutter.init` completes:

- Flutter framework errors (`FlutterError`), including widget and rendering
  failures that reach the framework error hook.
- Unhandled asynchronous errors from the current isolate through the platform
  error handler. Flutter versions before 3.3 may need the `runZonedGuarded`
  pattern shown below.
- Native Android and iOS crashes and native exceptions supported by the SDK.
- Handled exceptions sent explicitly at repository, HTTP, parsing, or job
  boundaries.
- Optional failed HTTP requests and sampled performance transactions when the
  application's HTTP client or Dio integration is instrumented.
- Release, distribution, environment, navigation, and bounded custom tags.

The SDK queues events locally and sends them while the app is running. A
successful SDK flush means its local queue drained; it does not prove that a
Flare worker or database persisted every item. For an acceptance check, send a
known event and confirm that the issue appears in the same Flare project.
Flutter may remove some layout errors in release builds before an error hook is
called, so a staging test cannot prove that every debug-only layout warning
will appear in production.

## Add the dependency

From the application repository:

```bash
flutter pub add sentry_flutter
```

Copy [`lib/flare_observability.dart`](lib/flare_observability.dart) into the
application or a shared internal Dart package. The helper was written against
the current `sentry_flutter` API; keep the package version managed by the
application's `pubspec.lock` and run `flutter pub upgrade` deliberately.

## Initialize before the first widget is built

The DSN is a public ingest identifier, but keep environment and release values
out of source when they identify private deployment details. Pass them through
the build or release pipeline:

```bash
flutter run \
  --dart-define=FLARE_SENTRY_DSN=https://public-key@flare.example/123456789012 \
  --dart-define=FLARE_ENV=staging \
  --dart-define=FLARE_RELEASE=mobile-app@2026.10.01+42
```

`main.dart`:

```dart
import 'package:flutter/material.dart';

import 'flare_observability.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await initializeFlareObservability(
    appRunner: () => runApp(const MyApp()),
    // Values can be omitted to use FLARE_SENTRY_DSN, FLARE_ENV, and
    // FLARE_RELEASE from --dart-define.
    tracesSampleRate: 0.1,
    captureFailedRequests: true,
  );
}

class MyApp extends StatelessWidget {
  const MyApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      // Add SentryNavigatorObserver() here when route breadcrumbs and
      // navigation transactions are wanted. See the notes below.
      home: const Scaffold(body: Center(child: Text('Flare example'))),
    );
  }
}
```

The `appRunner` callback matters: the SDK installs its error handlers around
the callback. Do not assign `FlutterError.onError` or
`PlatformDispatcher.instance.onError` after initialization without chaining the
existing handler, or the SDK may stop receiving those errors.

For a Flutter version before 3.3 that must own the outer error zone, use the
documented legacy form instead of wrapping `runApp` a second time:

```dart
import 'dart:async';

import 'package:flutter/widgets.dart';
import 'package:sentry_flutter/sentry_flutter.dart';

Future<void> main() async {
  runZonedGuarded(() async {
    await SentryFlutter.init(
      (options) {
        options.dsn = const String.fromEnvironment('FLARE_SENTRY_DSN');
        options.sendDefaultPii = false;
      },
      appRunner: () => runApp(const MyApp()),
    );
  }, (error, stackTrace) async {
    await Sentry.captureException(error, stackTrace: stackTrace);
  });
}
```

## Report handled failures

Not every failure should crash the app. Report the ones that are actionable and
continue the normal control flow:

```dart
import 'flare_observability.dart';

Future<User> loadUser(String id) async {
  try {
    return await api.fetchUser(id);
  } catch (error, stackTrace) {
    await captureFlareException(
      error,
      stackTrace: stackTrace,
      operation: 'user.fetch',
    );
    rethrow;
  }
}

Future<void> refresh() => runFlareGuarded(
      () => api.refresh(),
      operationName: 'sync.refresh',
    );
```

Do not include access tokens, authorization headers, email addresses, message
contents, document names, or unbounded IDs in exception messages, tags, or
breadcrumbs. The helper disables default PII. Add a `beforeSend` scrubber in
the `SentryFlutter.init` options for any application-specific fields, and
review it with the app's data owner before enabling production traffic.

## Navigation, network, and performance signals

These are opt-in because they can add volume and privacy considerations:

```dart
import 'package:sentry_flutter/sentry_flutter.dart';

MaterialApp(
  navigatorObservers: [SentryNavigatorObserver()],
  home: const MyHomePage(),
);
```

- Set a bounded `tracesSampleRate` (for example `0.1`) to collect sampled
  transactions. Leave it at `0` to disable performance tracing.
- Set `captureFailedRequests: true` and instrument the client used by the app
  (for example, wrap `package:http` with `SentryHttpClient`, or install the
  Sentry Dio integration) to capture failed HTTP requests. Confirm that request
  URLs do not contain credentials or user content, and add a
  transport/interceptor scrubber where required. The option alone cannot see a
  client that never passes requests through an SDK integration.
- Add `SentryNavigatorObserver` to `MaterialApp`, `WidgetsApp`, or
  `CupertinoApp` for navigation breadcrumbs and transactions. Use route names,
  not full URLs or user identifiers.
- For custom spans or transactions, use the `sentry_flutter` API directly and
  keep operation names low-cardinality (for example `sync.refresh`, not an ID).

## Mobile health: startup, frames, lifecycle, and API calls

The optional [`lib/flare_health.dart`](lib/flare_health.dart) helper and
[`lib/flare_flutter_health.dart`](lib/flare_flutter_health.dart) binding add
payload-free health signals to the same project. They use the existing native
metric and connector-attempt contracts; they do not add server fields or a
second mobile credential.

```bash
flutter pub add http
```

Create the reporter before `runApp`, then install the binding after
`WidgetsFlutterBinding.ensureInitialized()`:

```dart
import 'package:http/http.dart' as http;
import 'flare_health.dart';
import 'flare_flutter_health.dart';

final dsn = FlareDsn.parse(const String.fromEnvironment('FLARE_SENTRY_DSN'));
// appOfflineQueue is your app's bounded, persistent queue. It stores only the
// normalized metric or connector attempt in FlareDeliveryFailure, never the
// DSN, HTTP response, exception, or credentials.
final health = FlareHealthReporter(
  // Keep these byte-for-byte aligned with initializeFlareObservability below.
  // They become bounded metric labels for release-specific thresholds.
  environment: const String.fromEnvironment('FLARE_ENV', defaultValue: 'development'),
  release: const String.fromEnvironment('FLARE_RELEASE'),
  metricSink: dsn == null ? null : (point) async {
    final response = await http.post(
      dsn.metricEndpoint,
      headers: {'content-type': 'application/json'},
      body: encodeFlareMetricBatch([point]),
    ).timeout(const Duration(seconds: 5));
    if (!flareMetricResponseAccepted(response.statusCode, response.body, 1)) {
      throw const FormatException('metric delivery was not accepted');
    }
  },
  connectorSink: dsn == null ? null : (attempt) async {
    final response = await http.post(
      dsn.connectorEndpoint,
      headers: {'content-type': 'application/json'},
      body: encodeFlareConnectorAttempt(attempt),
    ).timeout(const Duration(seconds: 5));
    if (!flareConnectorResponseAccepted(response.statusCode, response.body)) {
      throw const FormatException('connector delivery was not accepted');
    }
  },
  onDeliveryFailure: (failure) => appOfflineQueue.enqueue(failure),
);

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  final healthBinding = installFlareFlutterHealth(health);
  await initializeFlareObservability(appRunner: () => runApp(const MyApp()));
  // Keep healthBinding alive for the process lifetime. Dispose it when the
  // application replaces its WidgetsBinding.
}
```

The binding records one first-frame startup duration, aggregates up to 120
frame timings at a time (`frame.max_ms`, `jank_frames`, and `jank_rate`), and
records bounded lifecycle state counters. Wrap backend calls with
`health.trackApi('GET /orders', request)` to emit an API latency metric and a
payload-free connector attempt. Public DSN reports are labeled
`client_reported` and cannot claim a registered provider installation. A
server-side provider adapter owns `server_observed` receipts and the
installation ID. The operation is reduced to a stable token;
request URLs, response bodies, exception messages, credentials, and user data
never enter these signals. A failed transport is swallowed so telemetry cannot
break the app. The sample checks HTTP 200 with `accepted == 1` for its one
metric and HTTP 202 with `accepted == true` for a connector attempt. Those
responses acknowledge the signal write; they do not prove the separate ingest
receipt completion was written. A timeout can occur after Flare commits, so a
retry may create another signal. The `onDeliveryFailure` callback receives only
the normalized signal, not the transport exception, response body, URL, or DSN.
For HTTP clients such as `package:http` that return 4xx/5xx responses without
throwing, pass `statusCodeOf: (response) => response.statusCode`. The helper
then counts that response as an API error and records its status class while
returning the response unchanged. Without this callback, only thrown failures
are counted as errors.
Implement `appOfflineQueue.enqueue` with bounded persistent storage and replay
with backoff using the same response checks. Queue failures are also swallowed
to keep telemetry from breaking the app; monitor the queue's own health through
app diagnostics. If the DSN is absent, sinks are disabled and no failed item is
queued. Report queue depth with
`health.recordOfflineQueueDepth(appOfflineQueue.length)` after bounded changes (the
helper caps it at 10,000). `environment` and `release` are sanitized into
bounded `environment` and `release` labels on every metric (for example,
`mobile-app@1.8.0+42` becomes `mobile-app@1.8.0_42`); use those exact normalized
values in `metric_threshold.metric_labels` selectors so staging and production
series cannot mix.

Use stable metric names such as `client.flutter.startup_ms`,
`client.flutter.frame.max_ms`, `client.flutter.jank_rate`,
`client.flutter.offline_queue_depth`, and
`client.flutter.api_errors.<operation>` in `metric_threshold` rules. A mobile
process may be suspended or killed, so an app lifecycle signal is not an
availability check. Put a Flare check-in on the backend or scheduler that owns
background sync, and pair it with the app's queue depth and API connector
series. Run `flutter test test/flare_health_test.dart` from the copied helper's
Flutter test suite; it is intentionally copyable and does not change an app's
`pubspec.yaml`.

## Syntax checks and CI check-ins

Runtime telemetry does not replace compile or static analysis checks. Run these
in the app's CI pipeline and keep their output as build evidence:

```bash
flutter pub get
flutter analyze
flutter test
```

If the pipeline should page when it stops running, create one Flare monitor and
store its generated check-in URL as a CI secret. The URL is project-scoped and
already contains the public ingest key; never print it in CI logs. A minimal
bounded job wrapper is:

```bash
set -uo pipefail
status=ok

flutter analyze || status=error
flutter test || status=error

# FLARE_CHECKIN_URL is the generated URL, including sentry_key.
if ! curl --fail-with-body -sS -G "$FLARE_CHECKIN_URL" \
  --data-urlencode "status=$status" >/dev/null; then
  status=error
fi
test "$status" = ok
```

Use a separate check-in slug for each app and pipeline. A successful check-in
means the job reported its result; it does not prove that the app was deployed
or that the backend accepted runtime events. Pair it with the release event
and a post-deploy synthetic test.

## Releases and symbols

Always set `environment` and `release` consistently across the app and backend.
Use a unique release per build, such as `mobile-app@1.8.0+42`, so Flare can
group regressions by release.

Flare accepts Sentry events, JavaScript source-map artifacts, and bounded native
symbol artifacts. Upload the Flutter `--split-debug-info` `.symbols` output as
CI evidence with a member API key (the endpoint also accepts Android
`mapping.txt`/ELF and iOS dSYM or Breakpad bundles):

```bash
symbols_b64=$(base64 < build/split-debug-info/app.android-arm64.symbols | tr -d '\n')
curl -fsS -X POST "$FLARE_URL/api/projects/$FLARE_PROJECT_ID/symbol-artifacts" \
  -H "Authorization: Bearer $FLARE_MEMBER_KEY" \
  -H 'Content-Type: application/json' \
  --data "$(jq -n --arg release "$FLARE_RELEASE" --arg content "$symbols_b64" \
    '{release:$release,platform:"flutter",format:"dart_symbols",name:"app.android-arm64.symbols",content_base64:$content}')"
```

The response contains only the release, platform, format, size, SHA-256 digest,
and durable `validation_status`; symbol bytes never enter hot events or API
responses. Flutter `dart_symbols` uploads are checked for a recognized
executable/DWARF container at upload time. Android and iOS formats remain
`not_checked` until their parsers are enabled. Native upload acceptance proves
that the artifact is retained for the release, while format-specific Dart,
Android, and iOS frame resolvers are still pending. Keep the original event and
release tags and do not claim native frames are readable until a resolver has
verified them. Flutter web builds can use the existing source-map upload path
after a release is registered, subject to its size and artifact limits.

## Isolates and background work

The current isolate is instrumented by the SDK. Work in a separate Dart isolate
needs an error listener; use the SDK's `addSentryErrorListener()` extension when
the pinned package exposes it, or catch and forward the error explicitly with
`captureFlareException`. Background sync should also send a Flare check-in so a
silent worker can be distinguished from a healthy quiet app. A mobile client
cannot be a reliable always-on monitor while the OS has suspended or terminated
it; put the check-in on the backend or scheduler for availability guarantees.

## Verify the end-to-end path

1. Create a Flare project and copy its DSN; keep the DSN project-scoped.
2. Run the app with `FLARE_ENV` and `FLARE_RELEASE` set.
3. Trigger one deliberate test exception from a non-production debug action.
4. Inspect the SDK response/logs and confirm the issue, release, and stack trace
   in the matching Flare project.
5. Remove the test trigger and repeat on the signed staging build.
6. For Flutter web, upload the generated source map and verify one readable
   frame. For Android/iOS, retain symbol files and record native symbolication
   as pending until Flare supports that artifact type.

This example does not create a Flutter app or modify another application's
`pubspec.yaml`; it is a reusable integration boundary for apps outside the
Flare repository as well as internal apps.
