/// Bounded, payload-free health signals for Flutter and mobile applications.
///
/// This file has no Flutter or HTTP dependency. Applications can pass the
/// resulting JSON to Flare's native `/api/{projectID}/metrics` and
/// `/api/{projectID}/connector-attempts` routes with their existing HTTP
/// client. All values are normalized before they reach an application-owned
/// transport, so a failed transport cannot turn a user value into a metric or
/// connector dimension.
library flare_health;

import 'dart:async';
import 'dart:convert';
import 'dart:math';

const int maxFlareMetricNameLength = 128;
const int maxFlareMetricTokenLength = 64;
const int maxFlareMetricLabels = 8;
const int maxFlareMetricLabelValueLength = 64;
const int maxFlareMetricBatch = 32;
const int maxFlareMetricValue = 1000000000000;
const int maxFlareConnectorElapsedMs = 24 * 60 * 60 * 1000;
const int maxFlareOfflineQueueDepth = 10000;

final RegExp _safeTokenPattern = RegExp(r'^[a-z0-9][a-z0-9_.:-]*$');
final RegExp _emailPattern = RegExp(r'\b[^\s@]+@[^\s@]+\.[^\s@]+\b');
final RegExp _secretPattern = RegExp(
  r'(?:authorization|bearer|basic|cookie|password|secret|token|api[_-]?key)\s*[:=]\s*[^\s,;]+',
  caseSensitive: false,
);
final RegExp _queryPattern = RegExp(r'[?#].*$');
final RegExp _longIDPattern = RegExp(r'\b(?:[0-9a-f]{16,}|\d{2,})\b', caseSensitive: false);

/// A value accepted by Flare's native metric ingest contract.
///
/// Metric names and labels are deliberately reduced to low-cardinality tokens.
/// Do not put route parameters, account IDs, message text, or request URLs in
/// them. Put dimensions that need separate alerting in the metric name.
class FlareMetricPoint {
  FlareMetricPoint({
    required String name,
    required num value,
    String kind = 'gauge',
    Map<String, String>? labels,
    DateTime? observedAt,
  })  : name = sanitizeFlareMetricName(name),
        value = sanitizeFlareMetricValue(value),
        kind = _metricKind(kind),
        labels = sanitizeFlareMetricLabels(labels),
        observedAt = (observedAt ?? DateTime.now()).toUtc(),
        idempotencyKey = _newFlareMetricKey();

  final String name;
  final double value;
  final String kind;
  final Map<String, String> labels;
  final DateTime observedAt;
  /// Reuse this key and [requestBody] byte-for-byte on every retry. Flare's
  /// native metric endpoint commits the points and terminal receipt together.
  final String idempotencyKey;
  late final String requestBody = encodeFlareMetricBatch(<FlareMetricPoint>[this]);

  Map<String, Object> toJson() {
    final out = <String, Object>{
      'name': name,
      'kind': kind,
      'value': value,
      'timestamp': observedAt.toIso8601String(),
    };
    if (labels.isNotEmpty) out['labels'] = labels;
    return out;
  }
}

/// A single payload-free connector outcome matching Flare's connector API.
class FlareConnectorAttempt {
  FlareConnectorAttempt({
    required String provider,
    required String operation,
    required String channel,
    required String outcome,
    String? statusClass,
    String? errorCode,
    required int elapsedMs,
    int? quotaRemaining,
    int? retryAfterMs,
    int? syncLagMs,
  })  : provider = sanitizeFlareToken(provider, fallback: 'app'),
        operation = sanitizeFlareOperation(operation),
        channel = sanitizeFlareToken(channel, fallback: 'api'),
        outcome = outcome == 'error' ? 'error' : 'ok',
        statusClass = sanitizeFlareStatusClass(statusClass),
        errorCode = sanitizeFlareErrorCode(errorCode),
        elapsedMs = _clampInt(elapsedMs, 0, maxFlareConnectorElapsedMs),
        quotaRemaining = _optionalBounded(quotaRemaining, 0, 1000000000000000),
        retryAfterMs = _optionalBounded(retryAfterMs, 0, 7 * 24 * 60 * 60 * 1000),
        syncLagMs = _optionalBounded(syncLagMs, 0, 7 * 24 * 60 * 60 * 1000),
        idempotencyKey = _newFlareMetricKey();

  final String provider;
  final String operation;
  final String channel;
  final String outcome;
  final String statusClass;
  final String errorCode;
  final int elapsedMs;
  final int? quotaRemaining;
  final int? retryAfterMs;
  final int? syncLagMs;
  /// Persist this key together with [requestBody] for ambiguous retries.
  final String idempotencyKey;
  late final String requestBody = jsonEncode(toJson());

  Map<String, Object> toJson() {
    final out = <String, Object>{
      'provider': provider,
      'operation': operation,
      'channel': channel,
      'outcome': outcome,
      'elapsed_ms': elapsedMs,
    };
    if (statusClass.isNotEmpty) out['status_class'] = statusClass;
    if (errorCode.isNotEmpty) out['error_code'] = errorCode;
    if (quotaRemaining != null) out['quota_remaining'] = quotaRemaining!;
    if (retryAfterMs != null) out['retry_after_ms'] = retryAfterMs!;
    if (syncLagMs != null) out['sync_lag_ms'] = syncLagMs!;
    return out;
  }
}

/// A frame sample supplied by the Flutter binding adapter.
class FlareFrameTiming {
  FlareFrameTiming({required this.build, required this.raster});

  final Duration build;
  final Duration raster;

  int get frameMs {
    final buildMs = build.inMicroseconds / 1000.0;
    final rasterMs = raster.inMicroseconds / 1000.0;
    return (buildMs > rasterMs ? buildMs : rasterMs).ceil();
  }
}

typedef FlareMetricSink = FutureOr<void> Function(FlareMetricPoint point);
typedef FlareConnectorSink = FutureOr<void> Function(FlareConnectorAttempt attempt);

/// A retry item containing only a normalized health signal. Transport errors,
/// response bodies, endpoint URLs, and DSN credentials are never included.
class FlareDeliveryFailure {
  const FlareDeliveryFailure.metric(FlareMetricPoint point)
      : metric = point,
        connectorAttempt = null;

  const FlareDeliveryFailure.connector(FlareConnectorAttempt attempt)
      : metric = null,
        connectorAttempt = attempt;

  final FlareMetricPoint? metric;
  final FlareConnectorAttempt? connectorAttempt;
}

int _fallbackMetricKeySequence = 0;

String _newFlareMetricKey() {
  try {
    final random = Random.secure();
    return base64Url.encode(List<int>.generate(16, (_) => random.nextInt(256)))
        .replaceAll('=', '');
  } on UnsupportedError {
    // The key prevents accidental duplicate delivery; it is not an auth key.
    // Keep telemetry working on platforms without a secure random source.
    final sequence = _fallbackMetricKeySequence++;
    final random = Random();
    return 'flutter_${DateTime.now().microsecondsSinceEpoch.toRadixString(16)}_'
        '${sequence.toRadixString(16)}_${random.nextInt(0x100000000).toRadixString(16)}';
  }
}

typedef FlareDeliveryFailureHandler = FutureOr<void> Function(
    FlareDeliveryFailure failure);

/// Checks the native metric endpoint's persistence acknowledgement.
/// An HTTP success with a lower accepted count is a failed delivery.
bool flareMetricResponseAccepted(int statusCode, String body, int sentCount) {
  if (statusCode != 200 || sentCount < 1 || sentCount > maxFlareMetricBatch) {
    return false;
  }
  try {
    final decoded = jsonDecode(body);
    if (decoded is! Map<String, dynamic>) return false;
    final accepted = decoded['accepted'];
    return accepted is int &&
        accepted == sentCount &&
        decoded['rejected'] == 0 &&
        decoded['dropped'] == 0 &&
        decoded['receipt_id'] is String &&
        (decoded['receipt_id'] as String).isNotEmpty;
  } catch (_) {
    return false;
  }
}

/// Checks the native connector-attempt endpoint's durable receipt.
bool flareConnectorResponseAccepted(int statusCode, String body) {
  if (statusCode != 202) return false;
  try {
    final decoded = jsonDecode(body);
    return decoded is Map<String, dynamic> &&
        decoded['accepted'] == true &&
        decoded['receipt_id'] is String &&
        (decoded['receipt_id'] as String).isNotEmpty;
  } catch (_) {
    return false;
  }
}

/// Produces bounded metric JSON for the native metric endpoint.
///
/// The native API accepts either an array or `{"metrics": [...]}`. The latter
/// is used here and capped to keep an offline batch small.
String encodeFlareMetricBatch(Iterable<FlareMetricPoint> points) {
  final batch = points.take(maxFlareMetricBatch).map((point) => point.toJson()).toList();
  return jsonEncode(<String, Object>{'metrics': batch});
}

/// Produces one bounded connector-attempt JSON body.
String encodeFlareConnectorAttempt(FlareConnectorAttempt attempt) => attempt.requestBody;

/// Parses the public project DSN into the two native endpoint URLs.
///
/// A DSN is an ingest credential scoped to one project. The returned URIs are
/// suitable for an app-owned HTTP client; member/API keys must never be used
/// here. Invalid or ambiguous DSNs return `null` rather than guessing a host.
class FlareDsn {
  FlareDsn._(this.metricEndpoint, this.connectorEndpoint);

  final Uri metricEndpoint;
  final Uri connectorEndpoint;

  static FlareDsn? parse(String raw) {
    final input = raw.trim();
    if (input.isEmpty) return null;
    late Uri uri;
    try {
      uri = Uri.parse(input);
    } catch (_) {
      return null;
    }
    if ((uri.scheme != 'https' && uri.scheme != 'http') ||
        uri.host.isEmpty ||
        uri.password.isNotEmpty ||
        uri.query.isNotEmpty ||
        uri.fragment.isNotEmpty) {
      return null;
    }
    final key = uri.userInfo;
    if (!RegExp(r'^[A-Za-z0-9_-]{1,64}$').hasMatch(key)) return null;
    final path = uri.path.replaceFirst(RegExp(r'/+$'), '');
    final slash = path.lastIndexOf('/');
    if (slash < 0 || slash == path.length - 1) return null;
    final projectID = path.substring(slash + 1);
    if (!RegExp(r'^[A-Za-z0-9_-]{1,64}$').hasMatch(projectID)) return null;
    final basePath = slash > 0 ? path.substring(0, slash) : '';
    Uri endpoint(String resource) => uri.replace(
          path: '$basePath/api/$projectID/$resource',
          queryParameters: <String, String>{'sentry_key': key, 'sentry_version': '7'},
          userInfo: '',
        );
    return FlareDsn._(endpoint('metrics'), endpoint('connector-attempts'));
  }
}

/// Reporter for app-level startup, frame, lifecycle, queue, and API health.
///
/// Sinks are application-owned and may batch, retry, or persist to an offline
/// queue. Pass the same build `environment` and `release` values used by the
/// Sentry event SDK; they are copied as bounded labels onto every metric so a
/// threshold cannot mix staging and production builds. Reporter failures are
/// sent to [onDeliveryFailure] as normalized retry items, then swallowed so
/// telemetry cannot break the app. The callback must persist a bounded queue;
/// its own errors are also swallowed and cannot be recovered by this helper.
class FlareHealthReporter {
  FlareHealthReporter({
    this.metricSink,
    this.connectorSink,
    this.onDeliveryFailure,
    DateTime Function()? now,
    String? environment,
    String? release,
    Set<String> allowedOperations = const <String>{},
  })  : _now = now ?? DateTime.now,
        _metricLabels = _sanitizeFlareReporterContext(environment, release),
        _allowedOperations = Set<String>.unmodifiable(allowedOperations.take(64));

  final FlareMetricSink? metricSink;
  final FlareConnectorSink? connectorSink;
  final FlareDeliveryFailureHandler? onDeliveryFailure;
  final DateTime Function() _now;
  // Keep the build identity on every native health metric. The Sentry event
  // path already carries environment/release, but metrics are a separate
  // ingest shape and otherwise cannot be selected safely by a release alert.
  final Map<String, String> _metricLabels;
  // Only explicitly registered, static operation names become series labels.
  // A URL or user-provided operation falls back to one shared safe bucket.
  final Set<String> _allowedOperations;
  bool _startupRecorded = false;

  Future<void> recordStartup(Duration elapsed) async {
    if (_startupRecorded) return;
    _startupRecorded = true;
    await _metric('client.flutter.startup_ms', _nonNegativeMs(elapsed));
  }

  Future<void> recordLifecycle(String state) async {
    final normalized = sanitizeFlareToken(state, fallback: 'unknown');
    await _metric('client.flutter.lifecycle.$normalized', 1);
  }

  Future<void> recordOfflineQueueDepth(int depth) async {
    await _metric(
      'client.flutter.offline_queue_depth',
      _clampInt(depth, 0, maxFlareOfflineQueueDepth),
    );
  }

  /// Aggregates at most 120 frame timings into four low-cardinality metrics.
  Future<void> recordFrameTimings(
    Iterable<FlareFrameTiming> timings, {
    Duration jankThreshold = const Duration(milliseconds: 16),
  }) async {
    final samples = timings.take(120).toList();
    if (samples.isEmpty) return;
    final thresholdMs = (jankThreshold.inMicroseconds / 1000.0).ceil();
    var maxMs = 0;
    var jank = 0;
    for (final sample in samples) {
      final frameMs = _clampInt(sample.frameMs, 0, maxFlareMetricValue);
      if (frameMs > maxMs) maxMs = frameMs;
      if (frameMs > thresholdMs) jank++;
    }
    await _metric('client.flutter.frame.max_ms', maxMs);
    await _metric('client.flutter.jank_frames', jank);
    await _metric('client.flutter.jank_rate', jank / samples.length);
  }

  /// Measures an API call and emits the existing connector-attempt shape.
  ///
  /// Use [statusCodeOf] when the HTTP client returns 4xx/5xx responses instead
  /// of throwing. The response is returned unchanged while a status >= 400 is
  /// reported as an error. Only the sanitized operation, status class, and
  /// error type are sent. Exceptions are rethrown to preserve app behavior.
  Future<T> trackApi<T>(
    String operation,
    Future<T> Function() request, {
    String channel = 'api',
    int? successStatusCode,
    int? Function(T response)? statusCodeOf,
    String? Function(Object error)? errorCode,
  }) async {
    final started = Stopwatch()..start();
    final safeOperation = _allowedOperations.contains(operation)
        ? sanitizeFlareOperation(operation)
        : 'unknown_operation';
    late T value;
    try {
      value = await request();
    } catch (error) {
      final elapsed = _elapsedMs(started);
      await _metric('client.flutter.api_latency_ms.$safeOperation', elapsed);
      await _metric('client.flutter.api_errors.$safeOperation', 1);
      String? safeErrorCode;
      try {
        safeErrorCode = errorCode?.call(error);
      } catch (_) {
        // An application-owned classification callback must never replace the
        // actual request exception.
      }
      await _connector(
        FlareConnectorAttempt(
          provider: 'backend',
          operation: safeOperation,
          channel: channel,
          outcome: 'error',
          errorCode: safeErrorCode ?? error.runtimeType.toString(),
          elapsedMs: elapsed,
        ),
      );
      rethrow;
    }
    int statusCode;
    try {
      statusCode = statusCodeOf?.call(value) ?? successStatusCode ?? 200;
    } catch (_) {
      // Do not turn a successful request into an app failure or falsely report
      // a successful HTTP result when its status cannot be classified.
      return value;
    }
    final elapsed = _elapsedMs(started);
    final failed = statusCode >= 400 && statusCode <= 599;
    await _metric('client.flutter.api_latency_ms.$safeOperation', elapsed);
    if (failed) {
      await _metric('client.flutter.api_errors.$safeOperation', 1);
    }
    await _connector(
      FlareConnectorAttempt(
        provider: 'backend',
        operation: safeOperation,
        channel: channel,
        outcome: failed ? 'error' : 'ok',
        statusClass: _statusClass(statusCode),
        elapsedMs: elapsed,
      ),
    );
    return value;
  }

  Future<void> _metric(String name, num value) async {
    final sink = metricSink;
    if (sink == null) return;
    FlareMetricPoint? point;
    try {
      point = FlareMetricPoint(
        name: name,
        value: value,
        labels: _metricLabels,
        observedAt: _now(),
      );
      await sink(point);
    } catch (_) {
      if (point != null) {
        await _deliveryFailed(FlareDeliveryFailure.metric(point));
      }
    }
  }

  Future<void> _connector(FlareConnectorAttempt attempt) async {
    final sink = connectorSink;
    if (sink == null) return;
    try {
      await sink(attempt);
    } catch (_) {
      await _deliveryFailed(FlareDeliveryFailure.connector(attempt));
    }
  }

  Future<void> _deliveryFailed(FlareDeliveryFailure failure) async {
    final handler = onDeliveryFailure;
    if (handler == null) return;
    try {
      await handler(failure);
    } catch (_) {
      // A broken offline queue must not become an application failure.
    }
  }
}

String sanitizeFlareMetricName(String raw) {
  final value = _token(raw, fallback: 'client.flutter.unknown');
  return value.length > maxFlareMetricNameLength
      ? value.substring(0, maxFlareMetricNameLength)
      : value;
}

String sanitizeFlareOperation(String raw) {
  var value = raw.trim().toLowerCase().replaceAll(_queryPattern, '');
  // A caller may accidentally pass a concrete URL instead of a route name.
  // Keep only the method and first resource, never a later user-controlled
  // path component such as /users/alice or /orders/abc.
  final route = RegExp(r'^([a-z]+)\s+/([a-z][a-z0-9_-]*)(/.*)?$').firstMatch(value);
  if (route != null) {
    final suffix = route.group(3) == null ? '' : '_id';
    return sanitizeFlareToken('${route.group(1)}_${route.group(2)}$suffix',
        fallback: 'unknown_operation');
  }
  if (value.contains('/') || value.contains('@')) return 'unknown_operation';
  value = value.replaceAll(_longIDPattern, 'id');
  return sanitizeFlareToken(value, fallback: 'unknown_operation');
}

String sanitizeFlareToken(String raw, {String fallback = 'unknown'}) =>
    _token(raw, fallback: fallback);

String sanitizeFlareErrorCode(String? raw) {
  if (raw == null || raw.trim().isEmpty) return '';
  return sanitizeFlareToken(raw, fallback: 'unknown_error');
}

String sanitizeFlareStatusClass(String? raw) {
  if (raw == null) return '';
  final value = raw.trim().toLowerCase();
  return RegExp(r'^[1-5]xx$').hasMatch(value) ? value : '';
}

double sanitizeFlareMetricValue(num value) {
  final converted = value.toDouble();
  if (converted.isNaN || converted.isInfinite) return 0;
  if (converted > maxFlareMetricValue) return maxFlareMetricValue.toDouble();
  if (converted < -maxFlareMetricValue) return -maxFlareMetricValue.toDouble();
  return converted;
}

Map<String, String> sanitizeFlareMetricLabels(Map<String, String>? labels) {
  if (labels == null || labels.isEmpty) return const <String, String>{};
  final out = <String, String>{};
  for (final entry in labels.entries.take(maxFlareMetricLabels)) {
    final key = sanitizeFlareToken(entry.key, fallback: 'label');
    final value = key == 'environment' || key == 'release'
        ? _sanitizeFlareMetricSelectorValue(entry.value)
        : sanitizeFlareToken(entry.value, fallback: 'unknown');
    if (!_safeTokenPattern.hasMatch(key) || value.isEmpty) continue;
    out[key] = value.length > maxFlareMetricLabelValueLength
        ? value.substring(0, maxFlareMetricLabelValueLength)
        : value;
  }
  return Map<String, String>.unmodifiable(out);
}

String _sanitizeFlareMetricSelectorValue(String raw) {
  var value = raw.trim().toLowerCase();
  if (value.isEmpty) return 'unknown';
  if (RegExp(
    r'(?:authorization|bearer|basic|cookie|password|secret|token|api[_-]?key)',
    caseSensitive: false,
  ).hasMatch(value)) {
    return 'redacted';
  }
  value = value.replaceAll(_queryPattern, '');
  value = value.replaceAll(_emailPattern, 'redacted_email');
  value = value.replaceAll(_secretPattern, 'redacted');
  // Keep the selector vocabulary accepted by Flare's metric alert API. Build
  // metadata such as `@1.8.0+42` stays useful, while spaces and other input
  // punctuation collapse to a bounded token.
  value = value.replaceAll(RegExp(r'[^a-z0-9_.:@/\-]+'), '_');
  value = value.replaceAll(RegExp(r'_+'), '_');
  value = value.replaceFirst(RegExp(r'^[_:.\-]+'), '');
  if (value.isEmpty) return 'unknown';
  if (value.length > maxFlareMetricTokenLength) {
    value = value.substring(0, maxFlareMetricTokenLength);
  }
  return value;
}

Map<String, String> _sanitizeFlareReporterContext(String? environment, String? release) {
  final labels = <String, String>{};
  if (environment != null && environment.trim().isNotEmpty) {
    labels['environment'] = environment;
  }
  if (release != null && release.trim().isNotEmpty) {
    labels['release'] = release;
  }
  return sanitizeFlareMetricLabels(labels);
}

String _token(String raw, {required String fallback}) {
  var value = raw.trim().toLowerCase();
  if (value.isEmpty) return fallback;
  // Treat the whole token as sensitive when a caller accidentally supplies an
  // authorization or secret-shaped value. This covers both `token=x` and
  // `Authorization: Bearer x` forms without trying to parse credentials.
  if (RegExp(
    r'(?:authorization|bearer|basic|cookie|password|secret|token|api[_-]?key)',
    caseSensitive: false,
  ).hasMatch(value)) {
    return 'redacted';
  }
  value = value.replaceAll(_emailPattern, 'redacted_email');
  value = value.replaceAll(_secretPattern, 'redacted');
  value = value.replaceAll(_queryPattern, '');
  // Replace identifier-shaped numbers/hex strings even when a caller uses a
  // generic token field (for example, a queue label or provider dimension).
  value = value.replaceAll(_longIDPattern, 'id');
  value = value.replaceAll(RegExp(r'[^a-z0-9_.:-]+'), '_');
  value = value.replaceAll(RegExp(r'_+'), '_');
  value = value.replaceFirst(RegExp(r'^[_:.\-]+'), '');
  if (value.isEmpty) return fallback;
  if (value.length > maxFlareMetricTokenLength) {
    value = value.substring(0, maxFlareMetricTokenLength);
  }
  return _safeTokenPattern.hasMatch(value) ? value : fallback;
}

int _clampInt(int value, int min, int max) => value < min ? min : (value > max ? max : value);

int? _optionalBounded(int? value, int min, int max) =>
    value == null || value < min || value > max ? null : value;

String _metricKind(String kind) => kind == 'sum' || kind == 'histogram' ? kind : 'gauge';

int _nonNegativeMs(Duration value) => _clampInt(value.inMilliseconds, 0, maxFlareMetricValue);

int _elapsedMs(Stopwatch stopwatch) => _clampInt(stopwatch.elapsedMilliseconds, 0, maxFlareConnectorElapsedMs);

String _statusClass(int statusCode) => statusCode >= 100 && statusCode <= 599 ? '${statusCode ~/ 100}xx' : '2xx';
