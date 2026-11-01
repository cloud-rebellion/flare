import { expect, test } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { flareBrowserAssetName, installFlareBrowserCapture, scrubFlareBrowserPath } from '../src/lib/flare-client';
import { sourceMapNameForURL } from '../../examples/browser/source-map-name.mjs';
import { sourceMapBundleDigest, uploadBrowserSourceMaps } from '../../examples/browser/upload-source-maps.mjs';
import { stageSourceMaps } from '../scripts/stage-source-maps.mjs';

test('browser paths retain route shape without arbitrary tokens', () => {
	expect(scrubFlareBrowserPath('/sign/opaqueInviteToken')).toBe('/sign/:id');
	expect(scrubFlareBrowserPath('/sign/opaque%2FInviteToken')).toBe('/sign/:id/:id');
	expect(scrubFlareBrowserPath('/users/alice%40example.com')).toBe('/users/:id');
	expect(scrubFlareBrowserPath('/api/projects/customer-name/issues/12345')).toBe('/api/projects/:id/issues/:id');
	expect(scrubFlareBrowserPath('/api/orders/opaqueCustomerSlug')).toBe('/api/orders/:id');
	expect(scrubFlareBrowserPath('/api/events/alice%40example.com')).toBe('/api/events/:id');
	expect(scrubFlareBrowserPath('/api/stream/customer%ZZ')).toBe('/api/stream/:id');
	expect(scrubFlareBrowserPath('/_app/immutable/chunks/app.BhOPzaDi.js')).toBe('/_app/immutable/chunks/:id');
});

test('browser route templates are bounded', () => {
	const route = scrubFlareBrowserPath('/' + 'secret/'.repeat(100), 40);
	expect(route.length).toBeLessThanOrEqual(40);
	expect(route).not.toContain('secret');
});

test('browser asset identity matches the upload helper without exposing a script URL', async () => {
	const url = 'https://app.example/_app/immutable/chunks/app.BhOPzaDi.js?code=private#fragment';
	const origin = 'https://app.example';
	const savedDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
	try {
		Object.defineProperty(globalThis, 'document', { configurable: true, value: { scripts: [
			{ src: url },
			{ src: 'https://app.example/assets/index-AbCd1234.js?code=one' },
			{ src: 'https://app.example/assets/index-AbCd1234.js?code=two' }
		] } });
		const name = await flareBrowserAssetName(url, origin);
		expect(name).toBe(sourceMapNameForURL(url));
		expect(name).toMatch(/^flare-asset-sha256:[a-f0-9]{64}$/);
		expect(name).not.toContain('BhOPzaDi');
		expect(name).not.toContain('private');
		expect(await flareBrowserAssetName('https://app.example/sign/credential.js', origin)).toBeUndefined();
		expect(await flareBrowserAssetName('blob:https://app.example/credential', origin)).toBeUndefined();
		expect(await flareBrowserAssetName('https://other.example/_app/immutable/chunks/app.BhOPzaDi.js', origin)).toBeUndefined();
		expect(await flareBrowserAssetName('https://app.example/_app/immutable/chunks/token.AbCd1234.js', origin)).toBeUndefined();
		expect(await flareBrowserAssetName('https://app.example/assets/index-AbCd1234.js?code=one', origin)).toBe(
			await flareBrowserAssetName('https://app.example/assets/index-AbCd1234.js?code=two', origin)
		);
		expect(() => sourceMapNameForURL('https://app.example/sign/credential.js')).toThrow('compiled JavaScript asset');
	} finally {
		if (savedDocument) Object.defineProperty(globalThis, 'document', savedDocument);
		else Reflect.deleteProperty(globalThis, 'document');
	}
});

test('a realtime browser error sends opaque stack assets and a templated page', async () => {
	const saved = Object.fromEntries(['window', 'location', 'navigator', 'document'].map((key) =>
		[key, Object.getOwnPropertyDescriptor(globalThis, key)]));
	const listeners = new Map();
	let accept;
	const received = new Promise((resolve) => { accept = resolve; });
	const page = 'https://app.example/sign/credential-token';
	try {
		Object.defineProperty(globalThis, 'location', { configurable: true, value: {
			href: page, pathname: '/sign/credential-token', origin: 'https://app.example'
		} });
		Object.defineProperty(globalThis, 'window', { configurable: true, value: {
			addEventListener: (name, handler) => listeners.set(name, handler),
			removeEventListener: (name) => listeners.delete(name)
		} });
		Object.defineProperty(globalThis, 'navigator', { configurable: true, value: {
			sendBeacon: (_url, body) => { void body.text().then(accept); return true; }
		} });
		Object.defineProperty(globalThis, 'document', { configurable: true, value: { scripts: [
			{ src: 'https://app.example/_app/immutable/chunks/app.BhOPzaDi.js?code=private' }
		] } });

		const stop = installFlareBrowserCapture({ dsn: 'https://public@flare.example/1', release: 'release-123' });
		try {
			const error = new Error('Render failed');
			error.stack = 'Error: Render failed\n    at view (https://app.example/_app/immutable/chunks/app.BhOPzaDi.js?code=private:7:13)' +
			'\n    at route (https://app.example/sign/credential-token.js:8:2)' +
			'\n    at vendor (https://cdn.example/_app/immutable/chunks/app.BhOPzaDi.js:9:3)';
			listeners.get('flare:exception')({ detail: { error } });
			const payload = await received;
			const event = JSON.parse(payload);
			const frame = event.exception.values[0].stacktrace.frames[0];
			expect(event.release).toBe('release-123');
			expect(event.request.url).toBe('https://app.example/sign/:id');
			expect(frame.filename).toBe(sourceMapNameForURL('https://app.example/_app/immutable/chunks/app.BhOPzaDi.js'));
			expect(frame.lineno).toBe(7);
			expect(frame.colno).toBe(13);
			expect(event.exception.values[0].stacktrace.frames.slice(1).map(({ filename }) => filename)).toEqual(['unknown', 'unknown']);
			expect(payload).not.toContain('credential-token');
			expect(payload).not.toContain('BhOPzaDi');
			expect(payload).not.toContain('private');
		} finally {
			stop();
		}
	} finally {
		for (const [key, descriptor] of Object.entries(saved)) {
			if (descriptor) Object.defineProperty(globalThis, key, descriptor);
			else Reflect.deleteProperty(globalThis, key);
		}
	}
});

test('Flare dashboard canary sends one fixed handled error per Window and ignores event detail', async () => {
	const saved = Object.fromEntries(['window', 'location', 'navigator', 'document'].map((key) =>
		[key, Object.getOwnPropertyDescriptor(globalThis, key)]));
	const beacons = [];
	const options = { dsn: 'https://public@flare.example/1', release: 'flare-v1+g' + 'a'.repeat(40) };
	try {
		Object.defineProperty(globalThis, 'location', { configurable: true, value: {
			href: 'https://flare.example.com/issues/private-token?code=private',
			pathname: '/issues/private-token', origin: 'https://flare.example.com'
		} });
		Object.defineProperty(globalThis, 'navigator', { configurable: true, value: {
			sendBeacon: (_url, body) => { beacons.push(body); return true; }
		} });
		Object.defineProperty(globalThis, 'document', { configurable: true, value: { scripts: [] } });
		const firstPage = new EventTarget();
		Object.defineProperty(globalThis, 'window', { configurable: true, value: firstPage });
		const stop = installFlareBrowserCapture(options);
		const untrustedEvent = new Event('flare:browser-canary');
		Object.defineProperty(untrustedEvent, 'detail', { value: { error: new Error('private detail'), message: 'private detail' } });
		firstPage.dispatchEvent(untrustedEvent);
		firstPage.dispatchEvent(new Event('flare:browser-canary'));
		stop();
		const stopAgain = installFlareBrowserCapture(options);
		firstPage.dispatchEvent(new Event('flare:browser-canary'));
		stopAgain();

		const secondPage = new EventTarget();
		Object.defineProperty(globalThis, 'window', { configurable: true, value: secondPage });
		const stopSecond = installFlareBrowserCapture(options);
		secondPage.dispatchEvent(new Event('flare:browser-canary'));
		stopSecond();
		for (let i = 0; i < 20 && beacons.length < 2; i++) await new Promise((resolve) => setTimeout(resolve, 5));

		expect(beacons).toHaveLength(2);
		const payloads = await Promise.all(beacons.map(async (body) => JSON.parse(await body.text())));
		for (const event of payloads) {
			expect(event.message).toBe('Flare dashboard browser source-map canary v1');
			expect(event.exception.values[0].type).toBe('FlareBrowserCanary');
			expect(event.exception.values[0].stacktrace.frames.length).toBeGreaterThan(0);
			expect(event.request.url).toBe('https://flare.example.com/issues/:id');
			expect(event.release).toBe(options.release);
			expect(JSON.stringify(event)).not.toContain('private detail');
			expect(JSON.stringify(event)).not.toContain('private-token');
		}
	} finally {
		for (const [key, descriptor] of Object.entries(saved)) {
			if (descriptor) Object.defineProperty(globalThis, key, descriptor);
			else Reflect.deleteProperty(globalThis, key);
		}
	}
});

test('opaque canary frame names bind separately to both Flare UI origins', async () => {
	const savedDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
	const path = '/_app/immutable/chunks/flare-client.AbCdEf12.js';
	// Synthetic origins keep fixed hashes stable through public privacy filtering.
	const expected = new Map([
		['https://primary.synthetic.invalid', 'c56b888982c9dcc55edb1a5518e052f22adeabeaeb925ad5a16e89adc143ac89'],
		['https://secondary.synthetic.invalid', 'c3e1183e304841dcedf5b6707a7e010b7608fe819629bdd1efa2e2cbef285b2b']
	]);
	try {
		for (const [origin, hash] of expected) {
			const script = `${origin}${path}`;
			Object.defineProperty(globalThis, 'document', { configurable: true, value: { scripts: [{ src: script }, { src: `${script}?v=1` }] } });
			const opaque = `flare-asset-sha256:${hash}`;
			expect(await flareBrowserAssetName(script, origin)).toBe(opaque);
			expect(await flareBrowserAssetName(`${script}?v=1#fragment`, origin)).toBe(opaque);
			expect(sourceMapNameForURL(script)).toBe(opaque);
			expect(await flareBrowserAssetName(`${origin}/_app/immutable/chunks/forged.AbCdEf12.js`, origin)).toBeUndefined();
			expect(await flareBrowserAssetName(script, [...expected.keys()].find((other) => other !== origin))).toBeUndefined();
			expect(opaque).not.toContain('flare-client');
		}
	} finally {
		if (savedDocument) Object.defineProperty(globalThis, 'document', savedDocument);
		else Reflect.deleteProperty(globalThis, 'document');
	}
});

test('dynamic imports use script resource evidence, not arbitrary fetches', async () => {
	const savedDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
	const savedPerformance = Object.getOwnPropertyDescriptor(globalThis, 'performance');
	const url = 'https://app.example/assets/index-AbCd1234.js';
	try {
		Object.defineProperty(globalThis, 'document', { configurable: true, value: { scripts: [] } });
		Object.defineProperty(globalThis, 'performance', { configurable: true, value: {
			getEntriesByName: (name, type) => name === url && type === 'resource' ? [{ initiatorType: 'script' }] : []
		} });
		expect(await flareBrowserAssetName(url, 'https://app.example')).toBe(sourceMapNameForURL(url));
		Object.defineProperty(globalThis, 'performance', { configurable: true, value: {
			getEntriesByName: () => [{ initiatorType: 'fetch' }]
		} });
		expect(await flareBrowserAssetName(url, 'https://app.example')).toBeUndefined();
	} finally {
		if (savedDocument) Object.defineProperty(globalThis, 'document', savedDocument);
		else Reflect.deleteProperty(globalThis, 'document');
		if (savedPerformance) Object.defineProperty(globalThis, 'performance', savedPerformance);
		else Reflect.deleteProperty(globalThis, 'performance');
	}
});

test('the reusable example stays byte-for-byte aligned with the Flare client', () => {
	expect(readFileSync(new URL('../src/lib/flare-client.ts', import.meta.url), 'utf8')).toBe(
		readFileSync(new URL('../../examples/browser/flare-browser.ts', import.meta.url), 'utf8')
	);
});

test('CI stages private maps and uploads them under the frame name', async () => {
	const dir = await mkdtemp(join(tmpdir(), 'flare-map-'));
	const build = join(dir, 'build');
	const maps = join(dir, 'source-maps');
	const asset = '_app/immutable/chunks/app.BhOPzaDi.js';
	const sourceMap = '{"version":3,"sources":["src/app.ts"],"mappings":"AAAA"}';
	try {
		await mkdir(join(build, '_app/immutable/chunks'), { recursive: true });
		await writeFile(join(build, asset), 'throw Error("boom")');
		await writeFile(join(build, `${asset}.map`), sourceMap);
		await writeFile(join(build, 'style.css.map'), '{}');
		expect(await stageSourceMaps(build, maps)).toEqual([`${asset}.map`, 'style.css.map']);
		expect(existsSync(join(build, `${asset}.map`))).toBe(false);
		expect(existsSync(join(build, 'style.css.map'))).toBe(false);
		expect(existsSync(join(build, asset))).toBe(true);

		const uploads = [];
		let accepted;
		const fetcher = async (url, request) => {
			if (request.method === 'GET') {
				uploads.push({ url: url.href, method: request.method, authorization: request.headers.Authorization });
				return { status: 200, json: async () => accepted };
			}
			const body = JSON.parse(request.body);
			uploads.push({ url: url.href, method: request.method, authorization: request.headers.Authorization, body });
			accepted = {
				id: 'bundle-1', project_id: 'project-1', release: body.release, digest: body.digest,
				artifact_count: body.artifacts.length, created_at: '2026-10-05T00:00:00Z',
				total_bytes: body.artifacts.reduce((sum, item) => sum + Buffer.byteLength(item.content), 0),
				artifacts: body.artifacts.map((item) => ({
					id: 'artifact-1', release: body.release, name: item.name, size: Buffer.byteLength(item.content),
					parse_status: 'valid', created_at: '2026-10-05T00:00:00Z',
					content_sha256: createHash('sha256').update(item.content).digest('hex')
				}))
			};
			return { status: 201, json: async () => accepted };
		};
		const receipts = await uploadBrowserSourceMaps({
			mapsDir: maps, publicBase: 'https://app.example/', flareBase: 'https://flare.example/',
			projectID: 'project-1', release: 'release-123', memberKey: 'test-key', fetcher
		});
		expect(receipts).toEqual([{ id: 'artifact-1', name: sourceMapNameForURL(`https://app.example/${asset}`), release: 'release-123' }]);
		expect(uploads).toEqual([
			{ url: 'https://flare.example/api/projects/project-1/artifact-bundles', method: 'POST', authorization: 'Bearer test-key',
				body: { release: 'release-123', digest: sourceMapBundleDigest('release-123', [{ name: receipts[0].name, content: sourceMap }]),
					artifacts: [{ name: receipts[0].name, content: sourceMap }] } },
			{ url: 'https://flare.example/api/projects/project-1/artifact-bundles/bundle-1', method: 'GET', authorization: 'Bearer test-key' }
		]);
		await expect(uploadBrowserSourceMaps({
			mapsDir: maps, publicBase: 'https://app.example/', flareBase: 'https://flare.example/',
			projectID: 'project-1', release: 'release-123', memberKey: 'test-key',
			fetcher: async (_url, request) => {
				const { release, digest, artifacts } = JSON.parse(request.body);
				return { status: 201, json: async () => ({
					id: 'bundle-2', project_id: 'project-1', release, digest, artifact_count: 1, total_bytes: Buffer.byteLength(sourceMap),
					artifacts: [{ id: 'artifact-2', release, name: artifacts[0].name, parse_status: 'invalid_mappings' }]
				}) };
			}
		})).rejects.toThrow('valid matching artifact receipts');
	} finally {
		await rm(dir, { recursive: true, force: true });
	}
});

test('source-map bundle digest uses portable exact-byte framing', () => {
	expect(sourceMapBundleDigest('web-42', [{ name: 'one.js', content: '{"version":3,"sources":["app.ts"],"names":[],"mappings":"AAAA"}' }]))
		.toBe('sha256:bcafca89a915667a7f88e7849843c75734ff1b478e68e51328b05b079dfec36d');
});
