/// Flutter binding adapters for the payload-free health reporter.
library flare_flutter_health;

import 'dart:async';

import 'package:flutter/widgets.dart';

import 'flare_health.dart';

/// Wires startup, lifecycle, and frame timing callbacks into a reporter.
///
/// The returned handle must be disposed when the application replaces the
/// binding. A mobile app cannot be an always-on monitor while the OS suspends
/// or terminates it; use a backend check-in for availability guarantees.
FlareFlutterHealthHandle installFlareFlutterHealth(FlareHealthReporter reporter, {
  Duration jankThreshold = const Duration(milliseconds: 16),
}) {
  final handle = FlareFlutterHealthHandle._(reporter, jankThreshold);
  handle._install();
  return handle;
}

class FlareFlutterHealthHandle with WidgetsBindingObserver {
  FlareFlutterHealthHandle._(this._reporter, this._jankThreshold)
      : _started = Stopwatch()..start();

  final FlareHealthReporter _reporter;
  final Duration _jankThreshold;
  final Stopwatch _started;
  bool _disposed = false;

  void _install() {
    final binding = WidgetsBinding.instance;
    binding.addObserver(this);
    binding.addTimingsCallback(_onTimings);
    unawaited(binding.waitUntilFirstFrameRasterized.then((_) async {
      if (_disposed) return;
      _started.stop();
      // This future completes after rasterization, unlike a post-frame
      // callback, which only observes the end of the Dart rendering pipeline.
      await _reporter.recordStartup(_started.elapsed);
    }));
  }

  void _onTimings(List<FrameTiming> timings) {
    if (_disposed) return;
    _reporter.recordFrameTimings(
      timings.map(
        (timing) => FlareFrameTiming(
          build: timing.buildDuration,
          raster: timing.rasterDuration,
        ),
      ),
      jankThreshold: _jankThreshold,
    );
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (_disposed) return;
    final value = state.toString().split('.').last;
    _reporter.recordLifecycle(value);
  }

  void dispose() {
    if (_disposed) return;
    _disposed = true;
    final binding = WidgetsBinding.instance;
    binding.removeObserver(this);
    binding.removeTimingsCallback(_onTimings);
  }
}
