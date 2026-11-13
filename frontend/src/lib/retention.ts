// Pure retention logic for the Settings page. It mirrors internal/retention so
// the preview shown before a change matches what the partition manager will
// do: same UTC calendar (the server's `today`), same "strictly before the
// cutoff" rule, same bounds.
import type { RetentionColdMode, RetentionDay, RetentionPolicy, RetentionSettings } from './types';

const DAY_MS = 86_400_000;

/** The first KEPT day for a window of `days`, as YYYY-MM-DD, counted from `today` (UTC). */
export function cutoffDay(today: string, days: number): string {
  const t = Date.parse(today + 'T00:00:00Z');
  return new Date(t - days * DAY_MS).toISOString().slice(0, 10);
}

/** Build the policy the API expects from the form fields. */
export function toPolicy(hotDays: number, coldMode: RetentionColdMode, coldDays: number): RetentionPolicy {
  const p: RetentionPolicy = { hot_retention_days: Math.trunc(hotDays), cold_mode: coldMode };
  if (coldMode === 'days') p.cold_retention_days = Math.trunc(coldDays);
  return p;
}

/** Client-side copy of Policy.Validate. Returns a message, or null when valid. */
export function validatePolicy(p: RetentionPolicy, b: RetentionSettings['bounds']): string | null {
  const hot = p.hot_retention_days;
  if (!Number.isInteger(hot) || hot < b.hot_min || hot > b.hot_max) {
    return `Hot retention must be between ${b.hot_min} and ${b.hot_max} days.`;
  }
  if (p.cold_mode === 'days') {
    const cold = p.cold_retention_days ?? NaN;
    if (!Number.isInteger(cold) || cold < hot || cold > b.cold_max) {
      return `Archive retention must be between the hot retention (${hot}) and ${b.cold_max} days.`;
    }
  }
  return null;
}

/**
 * Whether moving from cur to next keeps less data in either tier (needs a
 * confirm). Same rule as retention.Reduces: "off" keeps the archive already on
 * disk, so moving into "days" from any other mode prunes it, and moving into
 * "off" stops archiving the days that age out next.
 */
export function reduces(cur: RetentionPolicy, next: RetentionPolicy): boolean {
  if (next.hot_retention_days < cur.hot_retention_days) return true;
  if (next.cold_mode === 'off') return cur.cold_mode !== 'off';
  if (next.cold_mode === 'days') {
    return cur.cold_mode !== 'days' || (next.cold_retention_days ?? 0) < (cur.cold_retention_days ?? 0);
  }
  return false;
}

export function samePolicy(a: RetentionPolicy, b: RetentionPolicy): boolean {
  return (
    a.hot_retention_days === b.hot_retention_days &&
    a.cold_mode === b.cold_mode &&
    (a.cold_mode !== 'days' || a.cold_retention_days === b.cold_retention_days)
  );
}

export interface RetentionPreview {
  /** Hot days that leave Postgres on the next pass. */
  hotDays: number;
  hotRows: number;
  hotBytes: number;
  /** Of those, the ones that end up in the archive (not deleted). */
  archivedDays: number;
  archivedRows: number;
  /** Rows deleted outright (archive off, or already past archive retention). */
  deletedRows: number;
  /** Archive days deleted on the next pass (local archive only). */
  coldDays: number;
  coldBytes: number;
  coldFiles: number;
  /** Archive pruning was asked for but Flare cannot do it here (S3 / no DuckDB). */
  coldPruneUnavailable: boolean;
  /** The cold tier is down and dropping unarchived is not allowed: nothing leaves Postgres. */
  hotPaused: boolean;
}

function archiveCutoff(today: string, p: RetentionPolicy): string | null {
  if (p.cold_mode !== 'days') return null;
  // Same clamps as retention.ShouldPruneCold: never inside the hot window.
  const days = Math.max(p.cold_retention_days ?? 0, p.hot_retention_days, 7);
  return cutoffDay(today, days);
}

/**
 * What the next retention pass would remove under `next`, from the inventory
 * the server returned. Approximate: hot row counts are planner estimates.
 */
export function previewRemoval(
  s: Pick<RetentionSettings, 'today' | 'hot' | 'cold' | 'cold_tier'> & Partial<Pick<RetentionSettings, 'allow_drop_without_export'>>,
  next: RetentionPolicy
): RetentionPreview {
  const out: RetentionPreview = {
    hotDays: 0, hotRows: 0, hotBytes: 0, archivedDays: 0, archivedRows: 0, deletedRows: 0,
    coldDays: 0, coldBytes: 0, coldFiles: 0, coldPruneUnavailable: false, hotPaused: false
  };
  // Same as partition.prune: an archiving policy with no cold tier either
  // keeps every aged day (the default) or, with FLARE_ALLOW_DROP_WITHOUT_EXPORT,
  // deletes them with no Parquet copy.
  const coldDown = s.cold_tier.kind === 'unavailable';
  if (coldDown && next.cold_mode !== 'off' && !s.allow_drop_without_export) {
    out.hotPaused = true;
    return out;
  }
  const exports = next.cold_mode !== 'off' && !coldDown;
  const hotCut = cutoffDay(s.today, Math.max(1, next.hot_retention_days));
  const coldCut = archiveCutoff(s.today, next);
  for (const d of s.hot) {
    // Never today or the future, whatever the window says.
    if (!(d.day < hotCut && d.day < s.today)) continue;
    out.hotDays++;
    out.hotRows += d.approx_rows;
    out.hotBytes += d.bytes;
    const archived = exports && !(coldCut !== null && d.day < coldCut);
    if (archived) {
      out.archivedDays++;
      out.archivedRows += d.approx_rows;
    } else {
      out.deletedRows += d.approx_rows;
    }
  }
  if (coldCut !== null) {
    if (!s.cold_tier.prune_supported) {
      out.coldPruneUnavailable = true;
    } else {
      for (const d of s.cold as RetentionDay[]) {
        if (d.day < coldCut) {
          out.coldDays++;
          out.coldBytes += d.bytes;
          out.coldFiles += d.files ?? 0;
        }
      }
    }
  }
  return out;
}

export function describePolicy(p: RetentionPolicy): string {
  const hot = `${p.hot_retention_days} days in Postgres`;
  if (p.cold_mode === 'off') return `${hot}, then deleted (no archive)`;
  if (p.cold_mode === 'days') return `${hot}, archive kept ${p.cold_retention_days} days`;
  return `${hot}, archive kept forever`;
}

export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}
