import { afterAll, expect, test } from 'bun:test';

// This unit test exercises loadSession's actual assignment/error paths. The
// built-browser suite separately exercises compiled Svelte lifecycle behavior.
const previousRune = globalThis.$state;
globalThis.$state = (value) => value;
const { session, loadSession } = await import('../src/lib/session.svelte');
const previousFetch = globalThis.fetch;
afterAll(() => { globalThis.fetch = previousFetch; globalThis.$state = previousRune; });
const user = { id: 'native-user', email: 'native@example.test', org_id: 'native-org', role: 'member' };

test('only actual signed-out response clears a known session', async () => {
  for (const status of [403, 500, 503]) {
    session.user = user;
    globalThis.fetch = async () => Response.json({ error: 'unavailable' }, { status });
    await loadSession();
    expect(session.user).toBe(user);
  }
  session.user = user;
  globalThis.fetch = async () => { throw new Error('network'); };
  await loadSession();
  expect(session.user).toBe(user);
  globalThis.fetch = async () => Response.json({});
  await loadSession();
  expect(session.user).toBe(user);
  globalThis.fetch = async () => Response.json({ error: 'not authenticated' }, { status: 401 });
  await loadSession();
  expect(session.user).toBeNull();
  expect(session.loaded).toBe(true);
});

test('a typed current native response replaces only the cached user', async () => {
  globalThis.fetch = async () => Response.json(user);
  await loadSession();
  expect(session.user).toEqual(user);
});
