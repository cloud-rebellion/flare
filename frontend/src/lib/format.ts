export function relativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '';
  const secs = Math.round((Date.now() - then) / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(iso).toLocaleDateString();
}

// relativeUnix is relativeTime for a unix-seconds timestamp. Returns '' for 0
// (the "no pulse" sentinel) so callers can render an explicit dash instead.
export function relativeUnix(sec: number): string {
  if (!sec) return '';
  return relativeTime(new Date(sec * 1000).toISOString());
}

// Decimal counter strings may exceed JavaScript's safe integer range. Keep
// them as integers through formatting so a reported unit is never rounded away.
export function formatExactQuantity(value: string, locale?: string): string {
  if (!/^[0-9]+$/.test(value)) return 'Unavailable';
  return new Intl.NumberFormat(locale).format(BigInt(value));
}

// levelColor returns Tailwind text classes for an event/issue level.
export function levelColor(level: string): string {
  switch (level) {
    case 'fatal':
      return 'text-red-400';
    case 'error':
      return 'text-rose-400';
    case 'warning':
    case 'warn':
      return 'text-amber-400';
    case 'info':
      return 'text-sky-400';
    case 'debug':
    case 'trace':
      return 'text-zinc-500';
    default:
      return 'text-zinc-400';
  }
}

export function statusColor(status: string): string {
  switch (status) {
    case 'resolved':
      return 'text-emerald-400 border-emerald-400/30 bg-emerald-400/10';
    case 'ignored':
      return 'text-zinc-400 border-zinc-500/30 bg-zinc-500/10';
    default:
      return 'text-amber-400 border-amber-400/30 bg-amber-400/10';
  }
}
