// Optional, dependency-free browser error reporting for the Flare dashboard.
//
// This deliberately stays small and disabled unless PUBLIC_FLARE_DSN is set.
// A browser DSN is an ingest credential by design, so the server's per-DSN
// rate limit and project alert rules remain the control plane. Do not put a
// member API key or any other secret in this module.

export interface FlareBrowserCaptureOptions {
	dsn?: string;
	environment?: string;
	release?: string;
	maxEventsPerMinute?: number;
	captureResourceErrors?: boolean;
}

/**
 * Stable metric names emitted by the optional RUM bridge. Keep these names
 * fixed: metric_threshold rules refer to the series by name and a renamed
 * series is indistinguishable from a client that stopped reporting.
 */
export const FLARE_RUM_METRICS = Object.freeze({
	lcpMs: 'browser.web_vital.lcp_ms',
	cls: 'browser.web_vital.cls',
	inpMs: 'browser.web_vital.inp_ms',
	requestLatencyMs: 'browser.api_request.latency_ms',
	requestFailure: 'browser.api_request.failure',
	realtimeConnectLatencyMs: 'browser.realtime.connect_latency_ms',
	realtimeFailure: 'browser.realtime.failure'
} as const);

export type FlareRumMetricName = (typeof FLARE_RUM_METRICS)[keyof typeof FLARE_RUM_METRICS];

export interface FlareBrowserRumOptions {
	dsn?: string;
	environment?: string;
	release?: string;
	/** Fraction of eligible pages/requests to sample, from 0 to 1. */
	sampleRate?: number;
	/** Total metric points (vitals and requests) sent per rolling minute. */
	maxMetricsPerMinute?: number;
	captureWebVitals?: boolean;
	captureRequests?: boolean;
	/** Capture same-origin XMLHttpRequest API timings and failures. */
	captureXHR?: boolean;
	/** Capture allow-listed WebSocket/EventSource failures and connect latency. */
	captureRealtime?: boolean;
	/** Additional allow-list for API URLs. The default only tracks same-origin /api/. */
	requestFilter?: (url: URL, init?: RequestInit) => boolean;
}

interface ParsedDsn {
	endpoint: string;
	metricsEndpoint: string;
}

interface StackFrame {
	filename?: string;
	function?: string;
	lineno?: number;
	colno?: number;
	in_app?: boolean;
}

const maxTextBytes = 2048;
const maxFrames = 32;
const duplicateWindowMs = 10_000;
const defaultMaxEventsPerMinute = 20;

function boundedText(value: unknown, fallback: string): string {
	const text = typeof value === 'string' ? value : fallback;
	const scrubbed = text
		.replace(/([?&](?:token|key|secret|password|authorization|code)=)[^&\s]+/gi, '$1[REDACTED]')
		.replace(/((?:password|secret|token|api[_-]?key|authorization|cookie|email)\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^,}\s]+)/gi, '$1[REDACTED]')
		.replace(/\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]+/gi, '[REDACTED]')
		.replace(/\b(?:sk|ghp|glpat|xoxb|xoxp|AIza)[A-Za-z0-9._-]{8,}\b/g, '[REDACTED]')
		.replace(/\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b/gi, '[REDACTED_EMAIL]');
	return scrubbed.length > maxTextBytes ? `${scrubbed.slice(0, maxTextBytes)}…` : scrubbed;
}

// Browser paths are untrusted. A nonnumeric signing token or customer slug can
// look like an ordinary word, so only these fixed route words may cross the
// public DSN boundary. Everything else becomes a template segment.
const safeRouteSegments = new Set([
	'_app', 'accept-invite', 'account', 'accounts', 'admin', 'alerts', 'api', 'app',
	'assets', 'auth', 'build', 'business-health', 'callbacks', 'chunks', 'connectors',
	'contacts', 'contracts', 'customers', 'dashboards', 'deals', 'docs', 'documents',
	'dsn', 'entry', 'files', 'forgot-password', 'immutable', 'integrations', 'issues',
	'jobs', 'login', 'logout', 'logs', 'members', 'metrics', 'monitors', 'nodes',
	'overview', 'pipelines', 'profile', 'projects', 'public', 'register', 'releases',
	'reset-password', 'runs', 'search', 'settings', 'sign', 'spans', 'static', 'tasks',
	'teams', 'traces', 'users', 'verify', 'webhooks', 'workflows'
]);

/** Return a bounded route template without arbitrary path segments. */
export function scrubFlareBrowserPath(raw: string, maxLength = 512): string {
	let path = raw;
	try {
		// Decode at most twice so encoded separators cannot hide path segments.
		for (let i = 0; i < 2; i++) {
			const decoded = decodeURIComponent(path);
			if (decoded === path) break;
			path = decoded;
		}
	} catch {
		// An invalid escape is still safe because unknown segments are replaced.
	}
	const segments = path.split('/').filter(Boolean).slice(0, 12);
	const template = segments.map((segment) => safeRouteSegments.has(segment.toLowerCase())
		? segment.toLowerCase() : ':id');
	return (`/${template.join('/')}`).slice(0, Math.max(1, maxLength));
}

function safePage(): string | undefined {
	if (typeof location === 'undefined') return undefined;
	// Query strings, fragments, and arbitrary path segments can all carry tokens.
	const path = scrubFlareBrowserPath(location.pathname);
	return `${location.origin}${path}`;
}

function parseDsn(raw: string): ParsedDsn | null {
	try {
		const url = new URL(raw.trim());
		if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
		if (!url.username || url.password) return null;
		const path = url.pathname.replace(/\/+$/, '');
		const slash = path.lastIndexOf('/');
		const projectID = path.slice(slash + 1);
		if (!/^[A-Za-z0-9_-]{1,64}$/.test(projectID)) return null;
		const basePath = slash > 0 ? path.slice(0, slash) : '';
		const key = decodeURIComponent(url.username);
		if (!/^[A-Za-z0-9_-]{1,64}$/.test(key)) return null;
		const collector = `${url.origin}${basePath}/api/${encodeURIComponent(projectID)}`;
		const auth = `sentry_key=${encodeURIComponent(key)}`;
		const endpoint = `${collector}/events?${auth}&sentry_version=7`;
		const metricsEndpoint = `${collector}/metrics?${auth}`;
		return { endpoint, metricsEndpoint };
	} catch {
		return null;
	}
}

function eventID(): string {
	if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
		return crypto.randomUUID().replace(/-/g, '');
	}
	if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
		const bytes = new Uint8Array(16);
		crypto.getRandomValues(bytes);
		return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
	}
	return `${Date.now().toString(16)}${Math.random().toString(16).slice(2)}`.slice(0, 32).padEnd(32, '0');
}

function frameFilename(raw: string): string {
	try {
		const url = new URL(raw, typeof location === 'undefined' ? undefined : location.href);
		return `${url.origin}${scrubFlareBrowserPath(url.pathname)}`.slice(0, 512);
	} catch {
		return 'unknown';
	}
}

function parseStack(stack: string | undefined): StackFrame[] {
	if (!stack) return [];
	const frames: StackFrame[] = [];
	for (const line of stack.split('\n').slice(0, maxFrames)) {
		const trimmed = line.trim();
		let match = trimmed.match(/^at\s+(.*?)\s+\(?(.+?):(\d+)(?::(\d+))?\)?$/);
		if (!match) match = trimmed.match(/^(.*?)@(.+?):(\d+)(?::(\d+))?$/);
		if (!match) continue;
		const [, fn, file, lineNumber, columnNumber] = match;
		frames.push({
			filename: frameFilename(file ?? ''),
			function: boundedText(fn || '<anonymous>', '<anonymous>'),
			lineno: Number(lineNumber),
			colno: columnNumber ? Number(columnNumber) : undefined,
			in_app: true
		});
	}
	return frames;
}

function errorDetails(value: unknown): { type: string; message: string; stack?: string } {
	if (value instanceof Error) {
		return {
			type: boundedText(value.name, 'Error'),
			message: boundedText(value.message, 'Unhandled browser error'),
			stack: value.stack
		};
	}
	if (typeof value === 'string') return { type: 'UnhandledRejection', message: boundedText(value, 'Unhandled browser error') };
	try {
		return { type: 'UnhandledRejection', message: boundedText(JSON.stringify(value), 'Unhandled browser error') };
	} catch {
		return { type: 'UnhandledRejection', message: 'Unhandled browser error' };
	}
}

function installSender(dsn: ParsedDsn, options: FlareBrowserCaptureOptions): (value: unknown, stack?: string) => void {
	const maxEvents = Math.max(1, Math.min(options.maxEventsPerMinute ?? defaultMaxEventsPerMinute, 100));
	let windowStarted = Date.now();
	let sentInWindow = 0;
	const recent = new Map<string, number>();

	return (value, stack) => {
		const now = Date.now();
		if (now - windowStarted >= 60_000) {
			windowStarted = now;
			sentInWindow = 0;
			recent.clear();
		}
		if (sentInWindow >= maxEvents) return;
		const details = errorDetails(value);
		const signature = `${details.type}:${details.message}:${stack ?? details.stack ?? ''}`;
		const previous = recent.get(signature);
		if (previous && now - previous < duplicateWindowMs) return;
		recent.set(signature, now);
		sentInWindow++;

		const payload = JSON.stringify({
			event_id: eventID(),
			platform: 'javascript',
			level: 'error',
			environment: boundedText(options.environment, 'production'),
			release: boundedText(options.release, ''),
			timestamp: now / 1000,
			message: details.message,
			exception: {
				values: [{
					type: details.type,
					value: details.message,
					stacktrace: { frames: parseStack(stack ?? details.stack) }
				}]
			},
			request: safePage() ? { url: safePage() } : undefined,
			tags: { source: 'flare-browser-capture' }
		});

		// text/plain is a CORS-safelisted request, so cross-origin ingest does not
		// need a preflight. sendBeacon is the most reliable path during unload;
		// keepalive fetch covers browsers that reject the beacon queue.
		const body = new Blob([payload], { type: 'text/plain' });
		let queued = false;
		if (typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
			queued = navigator.sendBeacon(dsn.endpoint, body);
		}
		if (!queued && typeof fetch === 'function') {
			void fetch(dsn.endpoint, {
				method: 'POST',
				body: payload,
				headers: { 'Content-Type': 'text/plain' },
				mode: 'no-cors',
				credentials: 'omit',
				keepalive: true
			}).catch(() => {
				// Reporting must never surface as an application error or recurse.
			});
		}
	};
}

interface FlareBoundaryEventDetail {
	error?: unknown;
	componentStack?: string;
}

/**
 * Install bounded browser error capture once.
 *
 * The bridge intentionally uses the Sentry envelope shape but no browser SDK:
 * this keeps first-party static frontends small and avoids putting an API key
 * in the bundle. The DSN is an ingest-only credential and is rate limited by
 * Flare. React error boundaries can report handled render failures by
 * dispatching `flare:exception` with `{ error, componentStack }`.
 */
export function installFlareBrowserCapture(options: FlareBrowserCaptureOptions): () => void {
	if (typeof window === 'undefined' || !options.dsn?.trim()) return () => {};
	const dsn = parseDsn(options.dsn);
	if (!dsn) return () => {};
	const marker = '__flareBrowserCaptureInstalled';
	if ((window as Window & { [marker]?: boolean })[marker]) return () => {};
	(window as Window & { [marker]?: boolean })[marker] = true;
	const send = installSender(dsn, options);
	const onError = (event: ErrorEvent) => {
		// Resource failures also arrive as ErrorEvents. The capture-phase listener
		// below records those with a sanitized origin/path; do not emit a second
		// generic "Unhandled browser error" for the same failure.
		if (event.target instanceof Element) return;
		send(event.error ?? event.message, event.error?.stack);
	};
	const onRejection = (event: PromiseRejectionEvent) => send(event.reason);
	const onResourceError = (event: Event) => {
		const target = event.target;
		if (!(target instanceof Element)) return;
		const element = target.tagName.toLowerCase();
		const raw = element === 'script' ? target.getAttribute('src') :
			element === 'link' ? target.getAttribute('href') : target.getAttribute('src');
		let resource = element;
		try {
			if (raw) {
				const url = new URL(raw, location.href);
				resource = `${element}:${url.origin}${scrubFlareBrowserPath(url.pathname)}`.slice(0, 512);
			}
		} catch {
			resource = element;
		}
		send(new Error(`Failed to load browser resource (${resource})`));
	};
	const onBoundary = (event: Event) => {
		const detail = (event as CustomEvent<FlareBoundaryEventDetail>).detail;
		if (detail) send(detail.error ?? new Error('React error boundary caught an error'), detail.componentStack);
	};
	window.addEventListener('error', onError);
	window.addEventListener('unhandledrejection', onRejection);
	if (options.captureResourceErrors !== false) window.addEventListener('error', onResourceError, true);
	window.addEventListener('flare:exception', onBoundary);
	return () => {
		window.removeEventListener('error', onError);
		window.removeEventListener('unhandledrejection', onRejection);
		if (options.captureResourceErrors !== false) window.removeEventListener('error', onResourceError, true);
		window.removeEventListener('flare:exception', onBoundary);
		delete (window as Window & { [marker]?: boolean })[marker];
	};
}

const rumMetricNames = new Set<string>(Object.values(FLARE_RUM_METRICS));
const maxRumLabelBytes = 128;
const maxRumMetricValue = 24 * 60 * 60 * 1000;
const defaultRumSampleRate = 1;
const defaultMaxMetricsPerMinute = 20;

export interface FlareRumMetricPoint {
	name: FlareRumMetricName;
	kind: 'gauge';
	value: number;
	labels?: Record<string, string>;
	timestamp: string;
}

function clampFinite(value: number, min: number, max: number): number | undefined {
	if (!Number.isFinite(value)) return undefined;
	return Math.max(min, Math.min(max, value));
}

function normalizeSampleRate(value: number | undefined): number {
	return clampFinite(value ?? defaultRumSampleRate, 0, 1) ?? defaultRumSampleRate;
}

function normalizePositiveInt(value: number | undefined, fallback: number, max: number): number {
	if (!Number.isFinite(value)) return fallback;
	return Math.max(1, Math.min(max, Math.floor(value as number)));
}

function safeMetricRoute(raw: unknown): string | undefined {
	if (typeof raw !== 'string' || raw.trim() === '') return undefined;
	try {
		const fallbackOrigin = 'http://flare.invalid';
		const parsed = new URL(raw, typeof location === 'undefined' ? fallbackOrigin : location.href);
		// Cross-origin destinations are intentionally collapsed. A route path can
		// contain account ids even after query strings have been removed.
		if (!sameMetricOrigin(parsed)) return 'external';
		return scrubFlareBrowserPath(parsed.pathname, maxRumLabelBytes);
	} catch {
		return undefined;
	}
}

function sameMetricOrigin(url: URL): boolean {
	if (typeof location === 'undefined') return url.origin === 'http://flare.invalid';
	if (url.origin === location.origin) return true;
	// WebSocket URLs use a different scheme from the page even when they target
	// the same host. Treat the http/ws and https/wss pairs as same-origin for
	// route grouping, while retaining the host and port boundary.
	const websocketPair = (url.protocol === 'ws:' && location.protocol === 'http:') ||
		(url.protocol === 'wss:' && location.protocol === 'https:');
	return websocketPair && url.hostname === location.hostname && url.port === location.port;
}

function safeMetricLabels(raw: Record<string, unknown> | undefined): Record<string, string> | undefined {
	if (!raw) return undefined;
	const out: Record<string, string> = {};
	const safeToken = (key: string, maxBytes: number) => {
		const value = typeof raw[key] === 'string' ? raw[key].trim() : '';
		if (!value || value.length > maxBytes || !/^[A-Za-z0-9][A-Za-z0-9._:@/-]*$/.test(value)) return;
		out[key] = value;
	};
	safeToken('environment', 64);
	safeToken('release', 128);
	const method = typeof raw.method === 'string' ? raw.method.trim().toUpperCase() : '';
	if (/^[A-Z]{1,12}$/.test(method)) out.method = method;
	const statusClass = typeof raw.status_class === 'string' ? raw.status_class.trim().toLowerCase() : '';
	if (/^[1-5]xx$/.test(statusClass)) out.status_class = statusClass;
	const transport = typeof raw.transport === 'string' ? raw.transport.trim().toLowerCase() : '';
	if (/^(?:fetch|xhr|websocket|eventsource)$/.test(transport)) out.transport = transport;
	const state = typeof raw.state === 'string' ? raw.state.trim().toLowerCase() : '';
	if (/^(?:opening|open|reconnecting|closed|constructor)$/.test(state)) out.state = state;
	const route = safeMetricRoute(raw.route);
	if (route) out.route = route;
	return Object.keys(out).length > 0 ? out : undefined;
}

/**
 * Validate and scrub one point before it crosses the browser/DSN boundary.
 * This is exported so applications that use a custom transport can retain the
 * same allow-list and privacy contract as installFlareBrowserRum.
 */
export function normalizeFlareRumMetric(
	name: string,
	value: number,
	labels?: Record<string, unknown>,
	now = new Date()
): FlareRumMetricPoint | null {
	if (!rumMetricNames.has(name)) return null;
	const bounded = clampFinite(value, 0, maxRumMetricValue);
	if (bounded === undefined) return null;
	// CLS is unitless and should not be able to turn a typo into an unbounded
	// metric value. Browser entries are normally below 10; 100 is generous for
	// a page with many shifts while still making the client contract explicit.
	const finalValue = name === FLARE_RUM_METRICS.cls ? Math.min(100, bounded) : bounded;
	const normalizedLabels = safeMetricLabels(labels);
	return {
		name: name as FlareRumMetricName,
		kind: 'gauge',
		value: finalValue,
		...(normalizedLabels ? { labels: normalizedLabels } : {}),
		timestamp: now.toISOString()
	};
}

function monotonicNow(): number {
	if (typeof performance !== 'undefined' && typeof performance.now === 'function') return performance.now();
	return Date.now();
}

function requestURL(input: RequestInfo | URL): URL | undefined {
	try {
		if (typeof input === 'string' || input instanceof URL) {
			return new URL(String(input), typeof location === 'undefined' ? undefined : location.href);
		}
		return new URL(input.url, typeof location === 'undefined' ? undefined : location.href);
	} catch {
		return undefined;
	}
}

function requestMethod(input: RequestInfo | URL, init?: RequestInit): string {
	const raw = init?.method ?? (typeof Request !== 'undefined' && input instanceof Request ? input.method : 'GET');
	const method = typeof raw === 'string' ? raw.trim().toUpperCase() : 'GET';
	return /^[A-Z]{1,12}$/.test(method) ? method : 'GET';
}

function statusClass(status: number): string | undefined {
	if (!Number.isInteger(status) || status < 100 || status > 599) return undefined;
	return `${Math.floor(status / 100)}xx`;
}

function defaultRequestFilter(url: URL): boolean {
	if (!sameMetricOrigin(url)) return false;
	return url.pathname === '/api' || url.pathname.startsWith('/api/');
}

type FlarePerformanceObserverOptions = PerformanceObserverInit & { durationThreshold?: number };

function observePerformance(
	type: string,
	callback: (entries: PerformanceEntry[]) => void,
	options?: FlarePerformanceObserverOptions
): PerformanceObserver | undefined {
	if (typeof PerformanceObserver === 'undefined') return undefined;
	try {
		const observer = new PerformanceObserver((list) => callback(list.getEntries()));
		// `durationThreshold` is supported for event observers by browsers but is
		// absent from some DOM lib versions. Keep the extension local and cast only
		// at the platform boundary so application code remains strictly typed.
		observer.observe({ type, buffered: true, ...options } as PerformanceObserverInit);
		return observer;
	} catch {
		return undefined;
	}
}

/**
 * Install dependency-free browser RUM for the three allow-listed Web Vitals
 * and same-origin API request timing/failures. The helper uses the project's
 * public DSN only, emits native metric points, strips query strings and
 * identifier-shaped paths, and never includes request bodies or headers.
 */
export function installFlareBrowserRum(options: FlareBrowserRumOptions): () => void {
	if (typeof window === 'undefined' || !options.dsn?.trim()) return () => {};
	const dsn = parseDsn(options.dsn);
	if (!dsn) return () => {};
	const marker = '__flareBrowserRumInstalled';
	const markedWindow = window as Window & { [marker]?: boolean };
	if (markedWindow[marker]) return () => {};
	markedWindow[marker] = true;

	const sampleRate = normalizeSampleRate(options.sampleRate);
	const maxMetrics = normalizePositiveInt(options.maxMetricsPerMinute, defaultMaxMetricsPerMinute, 100);
	let windowStarted = Date.now();
	let sentInWindow = 0;
	const observers: PerformanceObserver[] = [];
	const cleanups: Array<() => void> = [];

	const send = (point: FlareRumMetricPoint) => {
		const payload = JSON.stringify([point]);
		const body = typeof Blob === 'function' ? new Blob([payload], { type: 'text/plain' }) : payload;
		let queued = false;
		if (typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
			queued = navigator.sendBeacon(dsn.metricsEndpoint, body);
		}
		if (!queued && typeof fetch === 'function') {
			void fetch(dsn.metricsEndpoint, {
				method: 'POST',
				body: payload,
				headers: { 'Content-Type': 'text/plain' },
				mode: 'no-cors',
				credentials: 'omit',
				keepalive: true
			}).catch(() => {
				// Reporting must never surface as an application error or recurse.
			});
		}
	};

	const report = (name: FlareRumMetricName, value: number, labels?: Record<string, unknown>) => {
		const now = Date.now();
		if (now - windowStarted >= 60_000) {
			windowStarted = now;
			sentInWindow = 0;
		}
		if (sentInWindow >= maxMetrics || sampleRate <= 0 || Math.random() > sampleRate) return;
		const point = normalizeFlareRumMetric(name, value, {
			environment: options.environment,
			release: options.release,
			...labels
		}, new Date(now));
		if (!point) return;
		sentInWindow++;
		send(point);
	};

	const shouldTrackRequest = (url: URL | undefined, init?: RequestInit): boolean => {
		if (!url || url.href === dsn.metricsEndpoint) return false;
		try {
			return options.requestFilter ? options.requestFilter(url, init) : defaultRequestFilter(url);
		} catch {
			return false;
		}
	};

	if (options.captureWebVitals !== false) {
		let lcp: number | undefined;
		let clsSessionValue = 0;
		let clsSessionStart = 0;
		let clsLastEntry = 0;
		let clsMax = 0;
		let inp = 0;
		observers.push(...[
			observePerformance('largest-contentful-paint', (entries) => {
				for (const entry of entries) lcp = Math.max(lcp ?? 0, entry.startTime);
			}),
			observePerformance('layout-shift', (entries) => {
				for (const raw of entries) {
					const entry = raw as PerformanceEntry & { value?: number; hadRecentInput?: boolean };
					if (entry.hadRecentInput || !Number.isFinite(entry.value)) continue;
					if (clsSessionStart === 0 || entry.startTime - clsLastEntry > 1000 || entry.startTime - clsSessionStart > 5000) {
						clsSessionStart = entry.startTime;
						clsSessionValue = 0;
					}
					clsSessionValue += entry.value ?? 0;
					clsMax = Math.max(clsMax, clsSessionValue);
					clsLastEntry = entry.startTime;
				}
			}),
			observePerformance('event', (entries) => {
				for (const raw of entries) {
					const entry = raw as PerformanceEntry & { duration?: number; interactionId?: number };
					if ((entry.interactionId ?? 0) > 0 && Number.isFinite(entry.duration)) inp = Math.max(inp, entry.duration ?? 0);
				}
			}, { durationThreshold: 16 })
		].filter((observer): observer is PerformanceObserver => observer !== undefined));

		let vitalSent = false;
		const flushVitals = () => {
			if (vitalSent) return;
			vitalSent = true;
			if (lcp !== undefined) report(FLARE_RUM_METRICS.lcpMs, lcp);
			if (clsMax > 0) report(FLARE_RUM_METRICS.cls, clsMax);
			if (inp > 0) report(FLARE_RUM_METRICS.inpMs, inp);
			for (const observer of observers) observer.disconnect();
		};
		const onVisibility = () => {
			if (document.visibilityState === 'hidden') flushVitals();
		};
		document.addEventListener('visibilitychange', onVisibility);
		window.addEventListener('pagehide', flushVitals);
		cleanups.push(() => {
			flushVitals();
			document.removeEventListener('visibilitychange', onVisibility);
			window.removeEventListener('pagehide', flushVitals);
		});
	}

	if (options.captureRequests !== false && typeof window.fetch === 'function') {
		const originalFetch = window.fetch;
		const wrappedFetch = function(this: Window, input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
			const url = requestURL(input);
			if (!shouldTrackRequest(url, init)) return originalFetch.call(this, input, init);
			const method = requestMethod(input, init);
			const route = url ? safeMetricRoute(url.href) : undefined;
			const started = monotonicNow();
			return originalFetch.call(this, input, init).then((response) => {
				const labels = { method, route, status_class: statusClass(response.status) };
				if (!response.ok) report(FLARE_RUM_METRICS.requestFailure, 1, labels);
				report(FLARE_RUM_METRICS.requestLatencyMs, Math.max(0, monotonicNow() - started), labels);
				return response;
			}, (error: unknown) => {
				const labels = { method, route };
				report(FLARE_RUM_METRICS.requestFailure, 1, labels);
				report(FLARE_RUM_METRICS.requestLatencyMs, Math.max(0, monotonicNow() - started), labels);
				return Promise.reject(error);
			});
		} as typeof window.fetch;
		window.fetch = wrappedFetch;
		cleanups.push(() => { window.fetch = originalFetch; });
	}

	if (options.captureXHR !== false) {
		const xhrPrototype = window.XMLHttpRequest?.prototype;
		if (xhrPrototype && typeof xhrPrototype.open === 'function' && typeof xhrPrototype.send === 'function') {
			interface XhrState {
				url?: URL;
				method: string;
				started: number;
				tracked: boolean;
				finished: boolean;
			}
			const states = new WeakMap<XMLHttpRequest, XhrState>();
			const listenerCleanups = new Set<() => void>();
			const originalOpen = xhrPrototype.open;
			const originalSend = xhrPrototype.send;
			const patchedOpen = function(this: XMLHttpRequest, ...args: Parameters<XMLHttpRequest['open']>) {
				const method = typeof args[0] === 'string' && /^[A-Za-z]{1,12}$/.test(args[0].trim()) ? args[0].trim().toUpperCase() : 'GET';
				const url = requestURL(args[1] as string | URL);
				const state: XhrState = {
					url,
					method,
					started: monotonicNow(),
					tracked: shouldTrackRequest(url, { method }),
					finished: false
				};
				states.set(this, state);
				try {
					return originalOpen.apply(this, args);
				} catch (error) {
					if (state.tracked) {
						report(FLARE_RUM_METRICS.requestFailure, 1, { method, route: url?.href, transport: 'xhr' });
						report(FLARE_RUM_METRICS.requestLatencyMs, Math.max(0, monotonicNow() - state.started), { method, route: url?.href, transport: 'xhr' });
					}
					state.finished = true;
					throw error;
				}
			} as XMLHttpRequest['open'];
			const patchedSend = function(this: XMLHttpRequest, ...args: Parameters<XMLHttpRequest['send']>) {
				const state = states.get(this);
				if (!state?.tracked || typeof this.addEventListener !== 'function') return originalSend.apply(this, args);
				const finish = () => {
					if (state.finished) return;
					state.finished = true;
					listenerCleanups.delete(detach);
					const labels = { method: state.method, route: state.url?.href, status_class: statusClass(this.status), transport: 'xhr' };
					if (this.status < 200 || this.status >= 300) report(FLARE_RUM_METRICS.requestFailure, 1, labels);
					report(FLARE_RUM_METRICS.requestLatencyMs, Math.max(0, monotonicNow() - state.started), labels);
				};
				const detach = () => {
					this.removeEventListener('loadend', finish);
					listenerCleanups.delete(detach);
				};
				this.addEventListener('loadend', finish);
				listenerCleanups.add(detach);
				try {
					return originalSend.apply(this, args);
				} catch (error) {
					finish();
					throw error;
				}
			} as XMLHttpRequest['send'];
			xhrPrototype.open = patchedOpen;
			xhrPrototype.send = patchedSend;
			cleanups.push(() => {
				for (const detach of listenerCleanups) detach();
				if (xhrPrototype.open === patchedOpen) xhrPrototype.open = originalOpen;
				if (xhrPrototype.send === patchedSend) xhrPrototype.send = originalSend;
			});
		}
	}

	if (options.captureRealtime === true) {
		const runtimeWindow = window as Window & { WebSocket?: typeof WebSocket; EventSource?: typeof EventSource };
		const realtimeCleanups = new Set<() => void>();
		type RealtimeKind = 'websocket' | 'eventsource';
		const realtimeURL = (value: unknown): URL | undefined => {
			if (typeof value !== 'string' && !(value instanceof URL)) return undefined;
			return requestURL(value);
		};
		const reportRealtimeConstructionFailure = (kind: RealtimeKind, value: unknown) => {
			const url = realtimeURL(value);
			if (!shouldTrackRequest(url, { method: kind === 'websocket' ? 'WS' : 'GET' })) return;
			report(FLARE_RUM_METRICS.realtimeFailure, 1, { transport: kind, route: url?.href, state: 'constructor' });
		};
		const trackRealtime = (kind: RealtimeKind, instance: WebSocket | EventSource, value: unknown) => {
			const url = realtimeURL(value);
			if (!shouldTrackRequest(url, { method: kind === 'websocket' ? 'WS' : 'GET' })) return;
			const started = monotonicNow();
			let opened = false;
			let lastFailure = -Infinity;
			const labels = (state: string) => ({ transport: kind, route: url?.href, state });
			const onOpen = () => {
				if (opened) return;
				opened = true;
				report(FLARE_RUM_METRICS.realtimeConnectLatencyMs, Math.max(0, monotonicNow() - started), labels('open'));
			};
			const onFailure = (state: string) => {
				const now = Date.now();
				if (now - lastFailure < duplicateWindowMs) return;
				lastFailure = now;
				report(FLARE_RUM_METRICS.realtimeFailure, 1, labels(state));
			};
			const onError = () => onFailure(opened ? 'open' : 'opening');
			const onClose = (event: Event) => {
				const code = (event as CloseEvent).code;
				if (code !== 1000 && code !== 1001) onFailure(opened ? 'closed' : 'opening');
			};
			instance.addEventListener('open', onOpen);
			instance.addEventListener('error', onError);
			if (kind === 'websocket') instance.addEventListener('close', onClose);
			const detach = () => {
				instance.removeEventListener('open', onOpen);
				instance.removeEventListener('error', onError);
				if (kind === 'websocket') instance.removeEventListener('close', onClose);
				realtimeCleanups.delete(detach);
			};
			realtimeCleanups.add(detach);
		};
		const wrapRealtime = <T extends WebSocket | EventSource>(kind: RealtimeKind, original: { new (...args: any[]): T }) => {
			const wrapped = new Proxy(original, {
				construct(target, args, newTarget) {
					try {
						const instance = Reflect.construct(target, args, newTarget) as T;
						try { trackRealtime(kind, instance, args[0]); } catch { /* instrumentation must never break app construction */ }
						return instance;
					} catch (error) {
						reportRealtimeConstructionFailure(kind, args[0]);
						throw error;
					}
				}
			}) as typeof original;
			return { original, wrapped };
		};
		const originalWebSocket = runtimeWindow.WebSocket;
		if (originalWebSocket) {
			const { wrapped } = wrapRealtime('websocket', originalWebSocket);
			runtimeWindow.WebSocket = wrapped as typeof WebSocket;
			cleanups.push(() => {
				for (const detach of realtimeCleanups) detach();
				if (runtimeWindow.WebSocket === wrapped) runtimeWindow.WebSocket = originalWebSocket;
			});
		}
		const originalEventSource = runtimeWindow.EventSource;
		if (originalEventSource) {
			const { wrapped } = wrapRealtime('eventsource', originalEventSource);
			runtimeWindow.EventSource = wrapped as typeof EventSource;
			cleanups.push(() => {
				for (const detach of realtimeCleanups) detach();
				if (runtimeWindow.EventSource === wrapped) runtimeWindow.EventSource = originalEventSource;
			});
		}
	}

	return () => {
		for (const cleanup of cleanups.splice(0)) cleanup();
		for (const observer of observers) observer.disconnect();
		delete markedWindow[marker];
	};
}
