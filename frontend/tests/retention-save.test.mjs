import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { reduces } from '../src/lib/retention';
import { CHANGED_ELSEWHERE, resetRetention, saveRetention, submitRetention } from '../src/lib/retention-save';

// A fake server with the real gate: 409 when the expected state moved or a
// reduction is not confirmed (same rule as changeRetention).
function server(initial, env) {
  const st = { stored: initial, updated: initial ? 't1' : null, n: 1 };
  const view = () => ({
    effective: st.stored ?? env,
    source: st.stored ? 'stored' : 'env',
    env_default: env,
    stored: st.stored,
    updated_at: st.updated,
    bounds: { hot_min: 7, hot_max: 365, cold_max: 3650 },
    today: '2026-11-08',
    cold_tier: { kind: 'local', prune_supported: true },
    hot: [],
    cold: [],
    last_pass: null
  });
  const conflict = () => Object.assign(new Error('conflict'), { status: 409 });
  const sent = [];
  const gate = (target, confirm, expected) => {
    const cur = view();
    if (JSON.stringify(expected.effective) !== JSON.stringify(cur.effective) || expected.updated_at !== cur.updated_at) throw conflict();
    if (reduces(cur.effective, target) && !confirm) throw conflict();
  };
  return {
    sent,
    view,
    otherAdminSaves(p) {
      st.stored = p;
      st.updated = 't' + ++st.n;
    },
    deps: {
      load: async () => view(),
      setRetention: async (p, confirm, expected) => {
        sent.push(['PUT', confirm]);
        gate(p, confirm, expected);
        st.stored = p;
        st.updated = 't' + ++st.n;
        return view();
      },
      resetRetention: async (confirm, expected) => {
        sent.push(['DELETE', confirm]);
        gate(env, confirm, expected);
        st.stored = null;
        st.updated = null;
        return view();
      }
    }
  };
}

const p = (hot, cold_mode = 'forever', cold_retention_days) =>
  cold_mode === 'days' ? { hot_retention_days: hot, cold_mode, cold_retention_days } : { hot_retention_days: hot, cold_mode };

test('a 409 from a stale view reloads, and the confirm click sends confirm_reduction: true', async () => {
  const srv = server(null, p(30));
  const seen = srv.view(); // the page loaded at env 30 days
  srv.otherAdminSaves(p(90)); // another admin raises to 90
  // 30 -> 60 looks like a raise in the stale view, so no confirm is asked for.
  const first = await saveRetention(srv.deps, seen, p(60), false);
  expect(first.kind).toBe('confirm');
  expect(first.settings.effective.hot_retention_days).toBe(90); // fresh data, not the stale view
  expect(first.changedElsewhere).toBe(true);
  // The admin confirms from the fresh view.
  const second = await saveRetention(srv.deps, first.settings, p(60), true);
  expect(second.kind).toBe('saved');
  expect(srv.sent).toEqual([
    ['PUT', false],
    ['PUT', true]
  ]);
});

test('an unconfirmed reduction reloads first, so the confirm quotes current data', async () => {
  const srv = server(null, p(30));
  let loads = 0;
  const deps = { ...srv.deps, load: async () => (loads++, srv.view()) };
  const out = await saveRetention(deps, srv.view(), p(7, 'off'), false);
  expect(out.kind).toBe('confirm');
  expect(loads).toBe(1);
  expect(srv.sent).toEqual([]); // nothing is written before the confirm
  expect((await saveRetention(deps, out.settings, p(7, 'off'), true)).kind).toBe('saved');
  expect(srv.sent).toEqual([['PUT', true]]);
});

test('turning the archive back on from off asks for the confirm (P0-1 in the UI)', async () => {
  const srv = server(p(30, 'off'), p(30));
  const out = await saveRetention(srv.deps, srv.view(), p(30, 'days', 30), false);
  expect(out.kind).toBe('confirm');
  expect(srv.sent).toEqual([]);
});

test('reset: a 409 reloads and the confirm click sends true', async () => {
  const srv = server(p(90), p(30));
  const seen = srv.view();
  srv.otherAdminSaves(p(120));
  const first = await resetRetention(srv.deps, seen, true); // confirmed against the stale 90
  expect(first.kind).toBe('confirm');
  expect(first.settings.effective.hot_retention_days).toBe(120);
  const second = await resetRetention(srv.deps, first.settings, true);
  expect(second.kind).toBe('saved');
  expect(srv.sent).toEqual([
    ['DELETE', true],
    ['DELETE', true]
  ]);
});

// The click transitions the component renders (fix audit P). Each of these
// fails if submitRetention stops deriving "confirmed" from the confirm box
// that is showing, or re-shows a confirm the fresh data no longer needs.
test('click flow: review, then confirm sends true and saves', async () => {
  const srv = server(null, p(30));
  let st = { settings: srv.view(), confirming: null };
  let r = await submitRetention(srv.deps, st, 'save', p(7, 'off'));
  expect(r.confirming).toBe('save');
  expect(r.adopt).toBe(false);
  expect(srv.sent).toEqual([]);
  st = { settings: r.settings, confirming: r.confirming };
  r = await submitRetention(srv.deps, st, 'save', p(7, 'off'));
  expect(srv.sent).toEqual([['PUT', true]]);
  expect(r.adopt).toBe(true);
  expect(r.confirming).toBeNull();
  expect(r.saved).toContain('Saved');
});

test('click flow: a raise never sends a confirmation', async () => {
  const srv = server(null, p(30));
  const r = await submitRetention(srv.deps, { settings: srv.view(), confirming: null }, 'save', p(90));
  expect(srv.sent).toEqual([['PUT', false]]);
  expect(r.adopt).toBe(true);
});

test('click flow: a confirm box for the OTHER action does not confirm this one', async () => {
  const srv = server(p(90), p(30));
  const r = await submitRetention(srv.deps, { settings: srv.view(), confirming: 'reset' }, 'save', p(7, 'off'));
  expect(srv.sent).toEqual([]);
  expect(r.confirming).toBe('save');
});

test('click flow: 409 because someone else changed it, and the change no longer lowers', async () => {
  const srv = server(null, p(30));
  const st = { settings: srv.view(), confirming: null };
  srv.otherAdminSaves(p(7, 'off')); // now 7 days; our 60 is a raise against that
  const r = await submitRetention(srv.deps, st, 'save', p(60));
  expect(r.confirming).toBeNull();
  expect(r.error).toBe(CHANGED_ELSEWHERE);
  expect(r.settings.effective.hot_retention_days).toBe(7);
});

test('click flow: confirmed reset that 409s re-confirms from fresh data, then succeeds', async () => {
  const srv = server(p(90), p(30));
  let st = { settings: srv.view(), confirming: null };
  let r = await submitRetention(srv.deps, st, 'reset');
  expect(r.confirming).toBe('reset');
  srv.otherAdminSaves(p(120));
  r = await submitRetention(srv.deps, { settings: r.settings, confirming: r.confirming }, 'reset');
  expect(r.confirming).toBe('reset');
  expect(r.error).toBe(CHANGED_ELSEWHERE);
  r = await submitRetention(srv.deps, { settings: r.settings, confirming: r.confirming }, 'reset');
  expect(r.adopt).toBe(true);
  expect(srv.sent).toEqual([
    ['DELETE', true],
    ['DELETE', true]
  ]);
});

test('the settings component delegates every click to submitRetention', () => {
  const src = readFileSync(new URL('../src/lib/components/RetentionSettings.svelte', import.meta.url), 'utf8');
  expect(src).toContain('submitRetention(saveDeps, { settings, confirming }, action');
  expect(src).not.toMatch(/api\.setRetention\(|api\.resetRetention\(|saveRetention\(|resetRetention\(saveDeps/);
  expect(src).not.toMatch(/const confirmed\s*=/);
});
