import { describe, expect, test } from 'bun:test';

import {
	FLARE_RUM_METRICS,
	installFlareBrowserRum,
	normalizeFlareRumMetric,
	scrubFlareBrowserPath
} from './flare-browser';

describe('Flare browser RUM metric contract', () => {
	test('scrubs plain, encoded, and malformed-escape email paths', () => {
		for (const raw of [
			'/users/alice@example.com/orders/42',
			'/users/alice%40example.com/orders/42',
			'/users/alice%2540example.com/orders/42',
			'/users/alice%40example.com%ZZ/orders/42'
		]) {
			const scrubbed = scrubFlareBrowserPath(raw);
			expect(scrubbed).toBe(raw.includes('%ZZ') ? '/users/[REDACTED_EMAIL]%ZZ/orders/:id' : '/users/[REDACTED_EMAIL]/orders/:id');
			expect(scrubbed).not.toContain('alice');
			expect(scrubbed).not.toContain('example.com');
		}
	});

	test('allows only stable names and scrubs request labels', () => {
		const point = normalizeFlareRumMetric(
			FLARE_RUM_METRICS.requestLatencyMs,
			123.4,
			{
				environment: 'production',
				release: 'web@abc123',
				method: ' get ',
				route: '/accounts/123456/orders/550e8400-e29b-41d4-a716-446655440000/person@example.com?token=secret',
				status_class: ' 5XX ',
				user: 'should-not-cross-the-boundary'
			},
			new Date('2026-10-02T12:00:00Z')
		);
		expect(point).toEqual({
			name: FLARE_RUM_METRICS.requestLatencyMs,
			kind: 'gauge',
			value: 123.4,
			labels: { environment: 'production', release: 'web@abc123', method: 'GET', route: '/accounts/:id/orders/:id/[REDACTED_EMAIL]', status_class: '5xx' },
			timestamp: '2026-10-02T12:00:00.000Z'
		});
		expect(JSON.stringify(point)).not.toContain('person@example.com');
		expect(normalizeFlareRumMetric('browser.web_vital.unknown', 1)).toBeNull();
		expect(normalizeFlareRumMetric(FLARE_RUM_METRICS.lcpMs, Number.NaN)).toBeNull();
	});

	test('bounds values and labels before transport', () => {
		const point = normalizeFlareRumMetric(
			FLARE_RUM_METRICS.cls,
			Number.MAX_VALUE,
			{ method: 'not a method', route: 'https://other.example/private/42' }
		);
		expect(point?.value).toBe(100);
		expect(point?.labels).toEqual({ route: 'external' });
	});
});

describe('Flare browser RUM request bridge', () => {
	test('records same-origin API latency and failure without request data', async () => {
		const runtime = globalThis as typeof globalThis & {
			window?: Window;
			document?: Document;
			location?: Location;
			navigator?: Navigator;
			PerformanceObserver?: typeof PerformanceObserver;
		};
		const oldWindow = runtime.window;
		const oldDocument = runtime.document;
		const oldLocation = runtime.location;
		const oldNavigator = runtime.navigator;
		const oldObserver = runtime.PerformanceObserver;
		const beacons: Array<{ url: string; body: Blob | string }> = [];
		const listeners = new Map<string, EventListener>();
		const originalFetch = async () => new Response('nope', { status: 503 });
		const fakeWindow = {
			fetch: originalFetch,
			addEventListener: (name: string, listener: EventListener) => listeners.set(name, listener),
			removeEventListener: (name: string) => listeners.delete(name)
		} as unknown as Window;
		runtime.window = fakeWindow;
		runtime.document = { visibilityState: 'visible', addEventListener() {}, removeEventListener() {} } as unknown as Document;
		runtime.location = new URL('https://app.example/') as unknown as Location;
		runtime.navigator = { sendBeacon: (url: string, body: Blob | string) => { beacons.push({ url, body }); return true; } } as unknown as Navigator;
		runtime.PerformanceObserver = undefined;
		try {
			const cleanup = installFlareBrowserRum({
				dsn: 'https://public-key@flare.example/123',
				maxMetricsPerMinute: 4,
				sampleRate: 1,
				captureWebVitals: false
			});
			await fakeWindow.fetch('/api/orders/42?email=person@example.com', { method: 'post' });
			cleanup();
			if (beacons.length !== 2) throw new Error(`expected latency and failure beacons, got ${beacons.length}`);
			const payloads = await Promise.all(beacons.map(async ({ body }) => JSON.parse(typeof body === 'string' ? body : await body.text())));
			expect(payloads.map((payload) => payload[0].name)).toEqual([
				FLARE_RUM_METRICS.requestFailure,
				FLARE_RUM_METRICS.requestLatencyMs
			]);
			expect(payloads[0][0].labels).toEqual({ method: 'POST', route: '/api/orders/:id', status_class: '5xx' });
			expect(JSON.stringify(payloads)).not.toContain('person@example.com');
		} finally {
			runtime.window = oldWindow;
			runtime.document = oldDocument;
			runtime.location = oldLocation;
			runtime.navigator = oldNavigator;
			runtime.PerformanceObserver = oldObserver;
		}
	});

	test('captures bounded XHR failures and realtime connection errors, then restores constructors', async () => {
		const runtime = globalThis as typeof globalThis & {
			window?: Window;
			document?: Document;
			location?: Location;
			navigator?: Navigator;
			PerformanceObserver?: typeof PerformanceObserver;
		};
		const oldWindow = runtime.window;
		const oldDocument = runtime.document;
		const oldLocation = runtime.location;
		const oldNavigator = runtime.navigator;
		const oldObserver = runtime.PerformanceObserver;
		const beacons: Array<{ url: string; body: Blob | string }> = [];
		const listeners = new Map<string, EventListener>();
		class FakeXHR {
			static prototypeMarker = true;
			status = 503;
			private listeners = new Map<string, EventListener>();
			open(_method: string, _url: string | URL) {}
			send() {
				this.listeners.get('loadend')?.call(this, new Event('loadend'));
			}
			addEventListener(name: string, listener: EventListener) { this.listeners.set(name, listener); }
			removeEventListener(name: string, listener: EventListener) {
				if (this.listeners.get(name) === listener) this.listeners.delete(name);
			}
		}
		class FakeWebSocket {
			private listeners = new Map<string, EventListener>();
			constructor(public url: string) {}
			addEventListener(name: string, listener: EventListener) { this.listeners.set(name, listener); }
			removeEventListener(name: string, listener: EventListener) {
				if (this.listeners.get(name) === listener) this.listeners.delete(name);
			}
			emit(name: string, event = new Event(name)) { this.listeners.get(name)?.call(this, event); }
		}
		class FakeEventSource {
			readyState = 0;
			private listeners = new Map<string, EventListener>();
			constructor(public url: string) {}
			addEventListener(name: string, listener: EventListener) { this.listeners.set(name, listener); }
			removeEventListener(name: string, listener: EventListener) {
				if (this.listeners.get(name) === listener) this.listeners.delete(name);
			}
			emit(name: string, event = new Event(name)) { this.listeners.get(name)?.call(this, event); }
		}
		const fakeWindow = {
			fetch: async () => new Response('ok'),
			XMLHttpRequest: FakeXHR,
			WebSocket: FakeWebSocket,
			EventSource: FakeEventSource,
			addEventListener: (name: string, listener: EventListener) => listeners.set(name, listener),
			removeEventListener: (name: string) => listeners.delete(name)
		} as unknown as Window;
		runtime.window = fakeWindow;
		runtime.document = { visibilityState: 'visible', addEventListener() {}, removeEventListener() {} } as unknown as Document;
		runtime.location = new URL('https://app.example/') as unknown as Location;
		runtime.navigator = { sendBeacon: (url: string, body: Blob | string) => { beacons.push({ url, body }); return true; } } as unknown as Navigator;
		runtime.PerformanceObserver = undefined;
		try {
			const cleanup = installFlareBrowserRum({
				dsn: 'https://public-key@flare.example/123',
				maxMetricsPerMinute: 20,
				sampleRate: 1,
				captureWebVitals: false,
				captureRealtime: true
			});
			const xhr = new (fakeWindow.XMLHttpRequest as unknown as { new (): FakeXHR })();
			xhr.open('post', '/api/orders/42?token=secret');
			xhr.send();
			const socket = new (fakeWindow.WebSocket as unknown as { new (url: string): FakeWebSocket })('wss://app.example/api/stream?token=secret');
			socket.emit('error');
			socket.emit('close', Object.assign(new Event('close'), { code: 1006 }));
			const source = new (fakeWindow.EventSource as unknown as { new (url: string): FakeEventSource })('/api/events/alice%40example.com');
			source.emit('error');
			cleanup();
			const payloads = await Promise.all(beacons.map(async ({ body }) => JSON.parse(typeof body === 'string' ? body : await body.text())));
			const points = payloads.map((payload) => payload[0]);
			expect(points.map((point) => point.name)).toEqual([
				FLARE_RUM_METRICS.requestFailure,
				FLARE_RUM_METRICS.requestLatencyMs,
				FLARE_RUM_METRICS.realtimeFailure,
				FLARE_RUM_METRICS.realtimeFailure
			]);
			expect(points[0].labels).toEqual({ method: 'POST', route: '/api/orders/:id', status_class: '5xx', transport: 'xhr' });
			expect(points[2].labels).toEqual({ transport: 'websocket', route: '/api/stream', state: 'opening' });
			expect(points[3].labels).toEqual({ transport: 'eventsource', route: '/api/events/[REDACTED_EMAIL]', state: 'opening' });
			expect(JSON.stringify(points)).not.toContain('secret');
			expect(JSON.stringify(points)).not.toContain('alice');
			expect((fakeWindow.XMLHttpRequest as unknown) === FakeXHR).toBe(true);
			expect((fakeWindow.WebSocket as unknown) === FakeWebSocket).toBe(true);
			expect((fakeWindow.EventSource as unknown) === FakeEventSource).toBe(true);
		} finally {
			runtime.window = oldWindow;
			runtime.document = oldDocument;
			runtime.location = oldLocation;
			runtime.navigator = oldNavigator;
			runtime.PerformanceObserver = oldObserver;
		}
	});
});
