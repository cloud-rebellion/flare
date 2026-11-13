import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import {
  cutoffDay,
  formatBytes,
  previewRemoval,
  reduces,
  toPolicy,
  validatePolicy
} from '../src/lib/retention';

const bounds = { hot_min: 7, hot_max: 365, cold_max: 3650 };
const today = '2026-11-08';

const inventory = {
  today,
  hot: [
    { day: '2026-10-20', approx_rows: 1000, bytes: 4096 },
    { day: '2026-10-31', approx_rows: 200, bytes: 1024 }, // first day past a 7 day window
    { day: '2026-11-01', approx_rows: 300, bytes: 1024 }, // oldest kept day
    { day: '2026-11-08', approx_rows: 50, bytes: 512 }, // today
    { day: '2026-11-10', approx_rows: 0, bytes: 256 } // pre-created future
  ],
  cold: [
    { day: '2026-01-01', approx_rows: 0, bytes: 9000, files: 4 },
    { day: '2026-10-09', approx_rows: 0, bytes: 100, files: 1 }
  ],
  cold_tier: { kind: 'local', prune_supported: true }
};

test('cutoff matches the server: strictly-before on the UTC calendar', () => {
  expect(cutoffDay(today, 7)).toBe('2026-11-01');
  expect(cutoffDay('2026-03-01', 1)).toBe('2026-02-28');
});

test('validation mirrors the server bounds', () => {
  expect(validatePolicy(toPolicy(7, 'off', 0), bounds)).toBeNull();
  expect(validatePolicy(toPolicy(6, 'off', 0), bounds)).not.toBeNull();
  expect(validatePolicy(toPolicy(366, 'forever', 0), bounds)).not.toBeNull();
  expect(validatePolicy(toPolicy(30, 'days', 29), bounds)).not.toBeNull();
  expect(validatePolicy(toPolicy(30, 'days', 30), bounds)).toBeNull();
  expect(validatePolicy(toPolicy(30, 'days', 3651), bounds)).not.toBeNull();
  // cold days are dropped unless the mode is "days"
  expect(toPolicy(7, 'off', 90)).toEqual({ hot_retention_days: 7, cold_mode: 'off' });
});

test('lowering either tier needs a confirmation, raising does not', () => {
  const cur = toPolicy(30, 'forever', 0);
  expect(reduces(cur, toPolicy(7, 'forever', 0))).toBe(true);
  expect(reduces(cur, toPolicy(30, 'off', 0))).toBe(true);
  expect(reduces(cur, toPolicy(30, 'days', 365))).toBe(true);
  expect(reduces(cur, toPolicy(60, 'forever', 0))).toBe(false);
  expect(reduces(toPolicy(7, 'off', 0), toPolicy(7, 'forever', 0))).toBe(false);
});

// Same table as internal/retention TestReduces, so the confirm the UI shows and
// the 409 the server returns are decided by one rule.
test('reduces matches the shared server table (off -> days is a reduction)', () => {
  const cases = JSON.parse(readFileSync(new URL('../../internal/retention/testdata/reduces_cases.json', import.meta.url), 'utf8'));
  expect(cases.length).toBeGreaterThan(10);
  for (const c of cases) {
    expect([c.name, reduces(c.cur, c.next)]).toEqual([c.name, c.want]);
  }
});

test('7 days with no archive: preview deletes the aged days, never today or the future', () => {
  const p = previewRemoval(inventory, toPolicy(7, 'off', 0));
  expect(p.hotDays).toBe(2);
  expect(p.hotRows).toBe(1200);
  expect(p.hotBytes).toBe(5120);
  expect(p.deletedRows).toBe(1200);
  expect(p.archivedRows).toBe(0);
  expect(p.coldDays).toBe(0); // "off" stops new exports, it does not wipe the archive
});

test('archive with a day count: aged hot days move to the archive, old archive days are pruned', () => {
  const p = previewRemoval(inventory, toPolicy(7, 'days', 30));
  expect(p.archivedDays).toBe(2);
  expect(p.deletedRows).toBe(0);
  expect(p.coldDays).toBe(1);
  expect(p.coldBytes).toBe(9000);
  expect(p.coldFiles).toBe(4);
});

test('a hot day already past archive retention counts as deleted, not archived', () => {
  const p = previewRemoval(inventory, toPolicy(7, 'days', 10));
  // 2026-10-20 < cutoff(10) = 2026-10-29: exported then pruned in the same pass
  expect(p.deletedRows).toBe(1000);
  expect(p.archivedRows).toBe(200);
});

test('S3 archive pruning is reported as unavailable instead of counted', () => {
  const p = previewRemoval({ ...inventory, cold_tier: { kind: 's3', prune_supported: false } }, toPolicy(7, 'days', 30));
  expect(p.coldPruneUnavailable).toBe(true);
  expect(p.coldDays).toBe(0);
});

test('byte formatting', () => {
  expect(formatBytes(0)).toBe('0 B');
  expect(formatBytes(512)).toBe('512 B');
  expect(formatBytes(1536)).toBe('1.5 KB');
  expect(formatBytes(50 * 1024 * 1024)).toBe('50 MB');
});

// P1-4: the preview follows FLARE_ALLOW_DROP_WITHOUT_EXPORT like the partition
// manager does. With the cold tier down it said "into the archive" for days
// that are actually deleted (allowed) or kept (not allowed).
test('cold tier down: allow-drop deletes aged days, otherwise nothing leaves Postgres', () => {
  const down = { ...inventory, cold_tier: { kind: 'unavailable', prune_supported: false } };
  const allowed = previewRemoval({ ...down, allow_drop_without_export: true }, toPolicy(7, 'forever', 0));
  expect(allowed.hotDays).toBe(2);
  expect(allowed.archivedRows).toBe(0);
  expect(allowed.deletedRows).toBe(1200);
  const kept = previewRemoval({ ...down, allow_drop_without_export: false }, toPolicy(7, 'forever', 0));
  expect(kept.hotPaused).toBe(true);
  expect(kept.hotDays).toBe(0);
  expect(kept.deletedRows).toBe(0);
});
