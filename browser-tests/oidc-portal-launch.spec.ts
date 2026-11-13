import { expect, test, type Page, type Route } from '@playwright/test';

// Exercise the real built-static login UI. Intercepted native/readiness/launch
// responses are a browser route contract, not provider SSO or account proof.
test.describe.configure({ mode: 'default', retries: 0 });

const nativeUser = {
  id: 'owned-flare-user', org_id: 'owned-flare-org',
  email: 'existing-reader@example.test', role: 'viewer'
};
const stopped = 'Single sign-on could not be checked. You can sign in below.';

type Reply = { status?: number; body?: unknown; raw?: string; contentType?: string; network?: boolean };
type Probe = 'me' | 'ready';
type Scenario = { me?: Reply; native?: Reply; ready?: Reply; hold?: Probe };
type Calls = {
  me: number; native: number; ready: number; launch: number; logout: number;
  intentConsumed: boolean; aborted: number; external: string[]; unexpected: string[];
  pageErrors: string[];
};
const fixtures = new WeakMap<Page, Calls>();

async function reply(route: Route, value: Reply) {
  if (value.network) return route.abort('failed');
  return route.fulfill({ status: value.status ?? 200,
    contentType: value.contentType ?? 'application/json',
    body: value.raw ?? JSON.stringify(value.body ?? {}) });
}

async function fixture(page: Page, scenario: Scenario = {}) {
  const origin = new URL(String(test.info().project.use.baseURL)).origin;
  const calls: Calls = { me: 0, native: 0, ready: 0, launch: 0, logout: 0,
    intentConsumed: true, aborted: 0, external: [], unexpected: [], pageErrors: [] };
  fixtures.set(page, calls);
  page.on('pageerror', (error) => calls.pageErrors.push(error.message.slice(0, 1000)));
  let release!: () => void;
  let entered!: () => void;
  let handled!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  const requested = new Promise<void>((resolve) => { entered = resolve; });
  const completed = new Promise<void>((resolve) => { handled = resolve; });
  const isPortalMe = (headers: Record<string, string>) => headers.accept === 'application/json';
  page.on('requestfailed', (request) => {
    const url = new URL(request.url());
    if (url.origin === origin && ((scenario.hold === 'me' && url.pathname === '/api/auth/me' && isPortalMe(request.headers())) ||
      (scenario.hold === 'ready' && url.pathname === '/api/auth/oidc/launch-status'))) calls.aborted++;
  });
  await page.route('**/*', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.origin !== origin) {
      calls.external.push(url.origin);
      return route.abort('blockedbyclient');
    }
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (request.method() !== 'GET') {
      if (url.pathname === '/api/auth/logout') calls.logout++;
      calls.unexpected.push(request.method() + ' ' + url.pathname);
      return route.fulfill({ status: 503, json: { error: 'unexpected test write' } });
    }
    let probe: Probe | undefined;
    let response: Reply;
    if (url.pathname === '/api/auth/me') {
      if (!isPortalMe(request.headers())) {
        calls.native++;
        return reply(route, scenario.native ?? scenario.me ?? { status: 401 });
      }
      calls.me++;
      calls.intentConsumed &&= !new URL(page.url()).searchParams.has('sso');
      probe = 'me';
      response = scenario.me ?? { status: 401, body: { error: 'not authenticated' } };
    } else if (url.pathname === '/api/auth/oidc/launch-status') {
      calls.ready++;
      probe = 'ready';
      response = scenario.ready ?? { body: { ready: true } };
    } else if (url.pathname === '/api/auth/oidc/launch') {
      calls.launch++;
      if (url.search !== '') calls.unexpected.push('launch query was forwarded');
      return route.fulfill({ contentType: 'text/html', body:
        '<!doctype html><title>Owned Flare launch fixture</title><p>Owned Flare launch reached</p>' });
    } else if (url.pathname === '/api/projects') {
      return route.fulfill({ json: [] });
    } else {
      calls.unexpected.push(request.method() + ' ' + url.pathname);
      return route.fulfill({ status: 503, json: { error: 'unexpected test API' } });
    }
    if (probe === scenario.hold) {
      entered();
      await held;
      try { await reply(route, response); }
      catch (error) { if (calls.aborted === 0) throw error; }
      finally { handled(); }
      return;
    }
    return reply(route, response);
  });
  return { calls, requested, completed, release };
}

test.afterEach(async ({ page }) => {
  const calls = fixtures.get(page);
  if (!calls) return;
  expect(calls.external, 'all browser traffic stays on the owned loopback origin').toEqual([]);
  expect(calls.pageErrors, 'the actual initial mount completes without browser exceptions').toEqual([]);
  expect(calls.unexpected).toEqual([]);
  expect(calls.logout).toBe(0);
});

test('typed native session opens Projects without starting OIDC', async ({ page }) => {
  const { calls } = await fixture(page, { me: { body: nativeUser } });
  await page.goto('/login?sso=1');
  await expect(page).toHaveURL((url) => url.pathname === '/projects');
  await expect(page.getByRole('heading', { name: 'Projects', exact: true })).toBeVisible();
  await expect(page.getByText(nativeUser.email, { exact: true })).toBeVisible();
  expect(calls.me).toBe(1);
  expect(calls.ready).toBe(0);
  expect(calls.launch).toBe(0);
  expect(calls.intentConsumed).toBe(true);
});

test('only signed-out plus typed readiness starts one fixed GET; Back and reload do not replay', async ({ page, baseURL }) => {
  const { calls } = await fixture(page);
  const origin = new URL(baseURL!).origin;
  await page.goto('/login?sso=1&return_url=https%3A%2F%2Fother.example%2F&redirect=%2F%2Fother.example');
  await expect(page).toHaveURL((url) => url.origin === origin && url.pathname === '/api/auth/oidc/launch' && url.search === '');
  await expect(page.getByText('Owned Flare launch reached', { exact: true })).toBeVisible();
  expect(calls.me).toBe(1); expect(calls.ready).toBe(1); expect(calls.launch).toBe(1);
  expect(calls.intentConsumed).toBe(true);
  await page.goBack();
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
  expect(new URL(page.url()).searchParams.has('sso')).toBe(false);
  await page.reload();
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
  expect(calls.me).toBe(1); expect(calls.ready).toBe(1); expect(calls.launch).toBe(1);
});

const unknownSessions: Array<{ name: string; response: Reply }> = [
  { name: '403', response: { status: 403 } },
  { name: '503', response: { status: 503 } },
  { name: 'network failure', response: { network: true } },
  { name: 'malformed JSON', response: { raw: '{broken' } },
  { name: 'untyped user', response: { body: { ...nativeUser, role: 'unknown' } } },
  { name: 'wrong content type', response: { contentType: 'text/plain', body: nativeUser } },
  { name: 'oversized response', response: { raw: JSON.stringify({ ...nativeUser, extra: 'x'.repeat(70000) }) } }
];
for (const scenario of unknownSessions) {
  test(`unknown portal session ${scenario.name} preserves known native state and never launches`, async ({ page }) => {
    const { calls } = await fixture(page, { me: scenario.response, native: { body: nativeUser } });
    await page.goto('/login?sso=1');
    await expect(page.getByText(stopped, { exact: true })).toBeVisible();
    await expect(page.getByText(nativeUser.email, { exact: true })).toBeVisible();
    expect(calls.me).toBe(1); expect(calls.ready).toBe(0); expect(calls.launch).toBe(0);
    expect(calls.intentConsumed).toBe(true);
    await page.reload();
    await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
    expect(calls.me).toBe(1); expect(calls.ready).toBe(0); expect(calls.launch).toBe(0);
  });
}

const unknownReadiness: Array<{ name: string; response: Reply }> = [
  { name: '403', response: { status: 403 } },
  { name: '503', response: { status: 503 } },
  { name: 'network failure', response: { network: true } },
  { name: 'malformed JSON', response: { raw: '{broken' } },
  { name: 'wrong content type', response: { contentType: 'text/plain', body: { ready: true } } },
  { name: 'false', response: { body: { ready: false } } },
  { name: 'missing', response: { body: {} } },
  { name: 'string true', response: { body: { ready: 'true' } } },
  { name: 'array', response: { body: [{ ready: true }] } },
  { name: 'oversized response', response: { raw: JSON.stringify({ ready: true, extra: 'x'.repeat(70000) }) } }
];
for (const scenario of unknownReadiness) {
  test(`unknown readiness ${scenario.name} stops without automatic retry`, async ({ page }) => {
    const { calls } = await fixture(page, { ready: scenario.response });
    await page.goto('/login?sso=1');
    await expect(page.getByText(stopped, { exact: true })).toBeVisible();
    expect(calls.me).toBe(1); expect(calls.ready).toBe(1); expect(calls.launch).toBe(0);
    expect(calls.intentConsumed).toBe(true);
    await page.reload();
    await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
    expect(calls.me).toBe(1); expect(calls.ready).toBe(1); expect(calls.launch).toBe(0);
  });
}

for (const error of ['sso_state', 'unknown-fixture-error', '']) {
  test(`callback error ${error || 'empty marker'} consumes intent without an automatic retry`, async ({ page }) => {
    const { calls } = await fixture(page);
    await page.goto('/login?sso=1&error=' + encodeURIComponent(error));
    await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
    if (error) await expect(page.getByText(error === 'sso_state'
      ? 'The sign-in link expired. Please try again.' : 'Single sign-on failed. Please try again.', { exact: true })).toBeVisible();
    expect(new URL(page.url()).searchParams.has('sso')).toBe(false);
    await page.reload();
    await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
    expect(calls.me).toBe(0); expect(calls.ready).toBe(0); expect(calls.launch).toBe(0);
  });
}

for (const intent of ['sso=0', 'sso=', 'sso=1&sso=1', 'sso=1&sso=0']) {
  test(`invalid or duplicated intent ${intent} never starts a probe`, async ({ page }) => {
    const { calls } = await fixture(page);
    await page.goto('/login?' + intent);
    await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
    expect(new URL(page.url()).searchParams.has('sso')).toBe(false);
    expect(calls.me).toBe(0); expect(calls.ready).toBe(0); expect(calls.launch).toBe(0);
  });
}

for (const hold of ['me', 'ready'] as const) {
  test(`leaving login aborts pending ${hold} and cannot launch from a late response`, async ({ page }) => {
    const control = await fixture(page, { hold });
    try {
      await page.goto('/login?sso=1');
      await control.requested;
      await page.getByRole('link', { name: /Documentation & API reference/ }).click();
      await expect(page).toHaveURL((url) => url.pathname === '/docs');
      // This must be component cancellation, not the helper's5s timeout.
      await expect.poll(() => control.calls.aborted, { timeout: 1500 }).toBeGreaterThan(0);
    } finally {
      control.release();
    }
    await control.completed;
    expect(control.calls.me).toBe(1);
    expect(control.calls.ready).toBe(hold === 'ready' ? 1 : 0);
    expect(control.calls.launch).toBe(0);
  });
}
