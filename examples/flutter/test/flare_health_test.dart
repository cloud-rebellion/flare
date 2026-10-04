import 'dart:convert';

import 'package:test/test.dart';

import '../lib/flare_health.dart';

void main() {
  test('metric values, names, labels, and payloads stay bounded and scrubbed', () {
    final point = FlareMetricPoint(
      name: 'GET /users/123?token=secret-alice@example.com',
      value: double.infinity,
      labels: {
        for (var i = 0; i < 12; i++) 'label-$i': 'alice@example.com',
      },
    );
    expect(point.name.length, lessThanOrEqualTo(maxFlareMetricNameLength));
    expect(point.value, maxFlareMetricValue.toDouble());
    expect(point.labels.length, maxFlareMetricLabels);
    final body = encodeFlareMetricBatch(List.filled(40, point));
    expect((jsonDecode(body)['metrics'] as List), hasLength(maxFlareMetricBatch));
    expect(body, isNot(contains('alice@example.com')));
    expect(body, isNot(contains('secret')));
    expect(body, isNot(contains('?token')));
  });

  test('connector attempts use bounded dimensions and existing fields only', () {
    final attempt = FlareConnectorAttempt(
      provider: 'backend',
      operation: 'GET /users/12345678901234567890?email=alice@example.com',
      channel: 'api',
      outcome: 'error',
      errorCode: 'Authorization: Bearer secret-value',
      elapsedMs: maxFlareConnectorElapsedMs * 4,
      retryAfterMs: -2,
    );
    final body = encodeFlareConnectorAttempt(attempt);
    final decoded = jsonDecode(body) as Map<String, dynamic>;
    expect(decoded.keys, containsAll(<String>['provider', 'operation', 'channel', 'outcome', 'elapsed_ms', 'error_code']));
    expect(decoded['elapsed_ms'], maxFlareConnectorElapsedMs);
    expect(decoded['retry_after_ms'], isNull);
    expect(body, isNot(contains('alice@example.com')));
    expect(body, isNot(contains('secret-value')));
    expect(body, isNot(contains('12345678901234567890')));
  });

  test('connector attempts can retain a safe registered installation id only', () {
    final attempt = FlareConnectorAttempt(
      provider: 'backend',
      connectionId: 'backend-prod-1',
      operation: 'sync',
      channel: 'api',
      outcome: 'ok',
      elapsedMs: 4,
    );
    final body = encodeFlareConnectorAttempt(attempt);
    expect(jsonDecode(body)['connection_id'], 'backend-prod-1');
    expect(
      FlareConnectorAttempt(
        provider: 'backend',
        connectionId: 'alice@example.com',
        operation: 'sync',
        channel: 'api',
        outcome: 'ok',
        elapsedMs: 4,
      ).connectionId,
      isEmpty,
    );
  });

  test('DSN parser produces only project scoped native endpoints', () {
    final dsn = FlareDsn.parse('https://public-key@flare.example/base/12345');
    expect(dsn, isNotNull);
    expect(dsn!.metricEndpoint.toString(), contains('/base/api/12345/metrics'));
    expect(dsn.connectorEndpoint.toString(), contains('/base/api/12345/connector-attempts'));
    expect(dsn.metricEndpoint.queryParameters['sentry_key'], 'public-key');
    expect(FlareDsn.parse('https://public-key:password@flare.example/12345'), isNull);
    expect(FlareDsn.parse('https://bad key@flare.example/12345'), isNull);
  });

  test('native response checks reject partial and malformed acknowledgements', () {
    expect(flareMetricResponseAccepted(200, '{"accepted":1}', 1), isTrue);
    expect(flareMetricResponseAccepted(200, '{"accepted":1}', 2), isFalse);
    expect(flareMetricResponseAccepted(202, '{"accepted":1}', 1), isFalse);
    expect(flareMetricResponseAccepted(200, '{"accepted":1.0}', 1), isFalse);
    expect(flareMetricResponseAccepted(200, '{"accepted":true}', 1), isFalse);
    expect(flareMetricResponseAccepted(200, '<html>ok</html>', 1), isFalse);
    expect(flareConnectorResponseAccepted(202, '{"accepted":true}'), isTrue);
    expect(flareConnectorResponseAccepted(200, '{"accepted":true}'), isFalse);
    expect(flareConnectorResponseAccepted(202, '{"accepted":false}'), isFalse);
    expect(flareConnectorResponseAccepted(202, '{"accepted":1}'), isFalse);
    expect(flareConnectorResponseAccepted(202, 'not json'), isFalse);
  });

  test('failed sinks send only normalized retry items to the app queue', () async {
    final failed = <FlareDeliveryFailure>[];
    final reporter = FlareHealthReporter(
      metricSink: (_) async => throw StateError('Bearer private-metric-secret'),
      connectorSink: (_) async => throw StateError('Bearer private-connector-secret'),
      onDeliveryFailure: (failure) {
        failed.add(failure);
      },
    );

    await reporter.recordLifecycle('resume?email=alice@example.com');
    final result = await reporter.trackApi<int>(
      'GET /orders/12345678901234567890?email=alice@example.com',
      () async => 42,
    );

    expect(result, 42);
    expect(failed.where((failure) => failure.metric != null), hasLength(2));
    expect(failed.where((failure) => failure.connectorAttempt != null), hasLength(1));
    final retried = jsonEncode(<Object>[
      for (final failure in failed)
        if (failure.metric != null) failure.metric!.toJson()
        else failure.connectorAttempt!.toJson(),
    ]);
    expect(retried, isNot(contains('alice@example.com')));
    expect(retried, isNot(contains('private-metric-secret')));
    expect(retried, isNot(contains('private-connector-secret')));
    expect(retried, isNot(contains('12345678901234567890')));
  });

  test('offline queue callback errors cannot change a successful API result', () async {
    final reporter = FlareHealthReporter(
      metricSink: (_) => throw StateError('metric transport failed'),
      connectorSink: (_) => throw StateError('connector transport failed'),
      onDeliveryFailure: (_) => throw StateError('offline queue failed'),
    );
    expect(await reporter.trackApi<int>('sync', () async => 7), 7);
  });

  test('returned HTTP error response is reported without changing app result', () async {
    final metrics = <FlareMetricPoint>[];
    final attempts = <FlareConnectorAttempt>[];
    final reporter = FlareHealthReporter(
      metricSink: metrics.add,
      connectorSink: attempts.add,
    );

    final responseStatus = await reporter.trackApi<int>(
      'GET /orders',
      () async => 503,
      statusCodeOf: (status) => status,
    );

    expect(responseStatus, 503);
    expect(attempts.single.outcome, 'error');
    expect(attempts.single.statusClass, '5xx');
    expect(metrics.map((point) => point.name), contains('client.flutter.api_errors.get_orders'));
  });

  test('reporter emits bounded frame and API health without surfacing sink errors', () async {
    final metrics = <FlareMetricPoint>[];
    final attempts = <FlareConnectorAttempt>[];
    final reporter = FlareHealthReporter(
      metricSink: (point) {
        metrics.add(point);
        throw StateError('offline');
      },
      connectorSink: (attempt) => attempts.add(attempt),
    );

    await reporter.recordFrameTimings(<FlareFrameTiming>[
      FlareFrameTiming(build: const Duration(milliseconds: 8), raster: const Duration(milliseconds: 20)),
      FlareFrameTiming(build: const Duration(milliseconds: 4), raster: const Duration(milliseconds: 5)),
    ]);
    expect(metrics.map((point) => point.name), containsAll(<String>[
      'client.flutter.frame.max_ms',
      'client.flutter.jank_frames',
      'client.flutter.jank_rate',
    ]));

    await expectLater(
      reporter.trackApi<void>(
        'GET /users/123?email=alice@example.com',
        () async {
          throw StateError('private user message');
        },
        connectionId: 'backend-prod',
      ),
      throwsStateError,
    );
    expect(attempts, hasLength(1));
    expect(attempts.single.outcome, 'error');
    expect(attempts.single.connectionId, 'backend-prod');
    expect(attempts.single.operation, isNot(contains('alice')));
    expect(attempts.single.errorCode, 'stateerror');
  });

  test('reporter attaches bounded environment and release labels to metrics', () async {
    final metrics = <FlareMetricPoint>[];
    final reporter = FlareHealthReporter(
      metricSink: (point) => metrics.add(point),
      environment: 'Production?tenant=alice@example.com',
      release: 'mobile-app@1.8.0+42',
    );

    await reporter.recordStartup(const Duration(milliseconds: 42));

    expect(metrics, hasLength(1));
    expect(metrics.single.labels, <String, String>{
      'environment': 'production',
      'release': 'mobile-app@1.8.0_42',
    });
    expect(jsonEncode(metrics.single.toJson()), isNot(contains('alice@example.com')));
  });
}
