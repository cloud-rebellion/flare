/// Small application-side wrapper for sending Flutter telemetry to Flare.
///
/// Flare accepts Sentry envelopes, so this helper deliberately depends on the
/// upstream `sentry_flutter` SDK instead of introducing a Flare-specific
/// transport. Keep this file in the application repository (or copy it into a
/// shared package) and pass the Flare project DSN at build time.
library flare_observability;

import 'dart:async';

import 'package:sentry_flutter/sentry_flutter.dart';

/// Initialises Sentry Flutter with defaults that are safe for a Flare project.
///
/// `SentryFlutter.init` installs the Flutter framework and platform error
/// handlers. `appRunner` is intentionally supplied to the SDK so those hooks
/// are installed before the first widget is built. Unhandled errors in the
/// current isolate and native Android/iOS crashes are captured by the SDK.
///
/// The DSN is normally provided with `--dart-define=FLARE_SENTRY_DSN=...`.
/// An empty DSN disables sending, which is useful for local development.
Future<void> initializeFlareObservability({
  required FutureOr<void> Function() appRunner,
  String? dsn,
  String? environment,
  String? release,
  String? dist,
  double tracesSampleRate = 0,
  bool captureFailedRequests = false,
}) async {
  if (tracesSampleRate < 0 || tracesSampleRate > 1) {
    throw ArgumentError.value(
      tracesSampleRate,
      'tracesSampleRate',
      'must be between 0 and 1',
    );
  }

  final resolvedDsn = dsn ??
      const String.fromEnvironment('FLARE_SENTRY_DSN', defaultValue: '');
  final resolvedEnvironment = environment ??
      const String.fromEnvironment('FLARE_ENV', defaultValue: 'development');
  final resolvedRelease = release ??
      const String.fromEnvironment('FLARE_RELEASE', defaultValue: '');
  final resolvedDist = dist ??
      const String.fromEnvironment('FLARE_DIST', defaultValue: '');

  await SentryFlutter.init(
    (options) {
      // An empty DSN is supported by Sentry as an explicit disabled mode.
      options.dsn = resolvedDsn;
      options.environment = resolvedEnvironment;
      if (resolvedRelease.isNotEmpty) options.release = resolvedRelease;
      if (resolvedDist.isNotEmpty) options.dist = resolvedDist;
      options.sendDefaultPii = false;
      options.attachStacktrace = true;
      options.captureFailedRequests = captureFailedRequests;
      options.tracesSampleRate = tracesSampleRate;
    },
    appRunner: appRunner,
  );
}

/// Sends a handled exception while keeping operation metadata on this event.
///
/// Use this at boundaries where an exception is handled locally but still
/// matters for production diagnostics. The per-event scope avoids leaking an
/// operation tag into unrelated events from concurrent requests or screens.
Future<void> captureFlareException(
  Object error, {
  StackTrace? stackTrace,
  String? operation,
}) async {
  await Sentry.captureException(
    error,
    stackTrace: stackTrace,
    withScope: operation == null
        ? null
        : (scope) {
            scope.setTag('operation', operation);
          },
  );
}

/// Sends an informational or warning event to Flare.
Future<void> captureFlareMessage(
  String message, {
  SentryLevel level = SentryLevel.info,
}) async {
  await Sentry.captureMessage(message, level: level);
}

/// Adds a stable tag to subsequent events in the current scope.
///
/// Do not use this for user input, email addresses, tokens, document names, or
/// other high-cardinality values. Prefer a bounded release, screen, or build
/// channel value.
Future<void> setFlareTag(String key, String value) async {
  await Sentry.configureScope((scope) {
    scope.setTag(key, value);
  });
}

/// Runs an operation and reports a handled failure before rethrowing it.
///
/// This is useful for repository, HTTP, and background-job boundaries where
/// the caller still needs normal error propagation.
Future<T> runFlareGuarded<T>(
  Future<T> Function() operation, {
  String? operationName,
}) async {
  try {
    return await operation();
  } catch (error, stackTrace) {
    await captureFlareException(
      error,
      stackTrace: stackTrace,
      operation: operationName,
    );
    rethrow;
  }
}
