import { expect, test } from 'bun:test';
import { checkPortalSession, consumePortalIntent, isSessionUser } from '../src/lib/ssoLaunch';

const user = { id: 'native-user', email: 'native@example.test', org_id: 'native-org', role: 'member' };
const json = (body, status = 200) => Response.json(body, { status });

test('explicit intent is consumed synchronously and cannot survive reload/back/error', () => {
  const url = new URL('https://app.example/login?sso=1&keep=1#local');
  const changes = [];
  expect(consumePortalIntent(url, (path) => changes.push(path))).toBe(true);
  expect(changes).toEqual(['/login?keep=1#local']);
  expect(consumePortalIntent(url, () => { throw new Error('consumed intent repeated'); })).toBe(false);
  for (const query of ['sso=1&error=sso_state', 'sso=1&sso=1', 'sso=true']) {
    const candidate = new URL('https://app.example/login?' + query);
    expect(consumePortalIntent(candidate, () => {})).toBe(false);
    expect(candidate.searchParams.has('sso')).toBe(false);
  }
});

test('server-verified native session wins without checking a provider', async () => {
  const calls = [];
  const result = await checkPortalSession(new AbortController().signal, async (path, options) => {
    calls.push(path);
    expect(options.method).toBe('GET');
    expect(options.credentials).toBe('same-origin');
    expect(options.cache).toBe('no-store');
    expect(options.redirect).toBe('error');
    return json(user);
  });
  expect(result).toEqual({ kind: 'session', user });
  expect(calls).toEqual(['/api/auth/me']);
});

test('only true signed-out plus typed ready response selects one fixed launch', async () => {
  const calls = [];
  const result = await checkPortalSession(new AbortController().signal, async (path) => {
    calls.push(path);
    return path === '/api/auth/me' ? json({ error: 'not authenticated' }, 401) : json({ ready: true, redirect: 'https://ignored.example' });
  });
  expect(result).toEqual({ kind: 'launch' });
  expect(calls).toEqual(['/api/auth/me', '/api/auth/oidc/launch-status']);
});

test('unknown native responses never probe readiness or alter native identity', async () => {
  for (const response of [json({}, 403), json({}, 503), json({}), new Response('<html>unexpected</html>'), json({ ...user, role: 1 })]) {
    const calls = [];
    const known = { ...user };
    expect(await checkPortalSession(new AbortController().signal, async (path) => { calls.push(path); return response; })).toEqual({ kind: 'stopped' });
    expect(calls).toEqual(['/api/auth/me']);
    expect(known).toEqual(user);
  }
  expect(isSessionUser(user)).toBe(true);
  expect(isSessionUser({ ...user, org_id: '' })).toBe(false);
});

test('unready, malformed, failed and oversized readiness stop without retries', async () => {
  for (const response of [json({ ready: false }), json({ ready: 'true' }), json(null), json({}, 503), json({}, 403), json({ ready: true, excess: 'x'.repeat(70000) })]) {
    const calls = [];
    expect(await checkPortalSession(new AbortController().signal, async (path) => {
      calls.push(path); return path === '/api/auth/me' ? json({}, 401) : response;
    })).toEqual({ kind: 'stopped' });
    expect(calls).toEqual(['/api/auth/me', '/api/auth/oidc/launch-status']);
  }
});

test('network and cancellation never launch, including an aborted in-flight probe', async () => {
  let calls = 0;
  expect(await checkPortalSession(new AbortController().signal, async () => { calls++; throw new Error('network'); })).toEqual({ kind: 'stopped' });
  expect(calls).toBe(1);
  const controller = new AbortController();
  const pending = checkPortalSession(controller.signal, async (_path, options) => new Promise((_resolve, reject) => {
    options.signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
  }));
  controller.abort();
  expect(await pending).toEqual({ kind: 'stopped' });
  expect(await checkPortalSession(controller.signal, async () => { throw new Error('must not request'); })).toEqual({ kind: 'stopped' });
});
