// Formatting helpers. Pure functions, so they are unit tested.

const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

/** "just now", "3 min ago", "in 7 min", "2 h ago", "4 days ago". */
export function relativeTime(iso: string | undefined, now: number = Date.now()): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const s = Math.round((t - now) / 1000);
  const abs = Math.abs(s);
  if (abs < 10) return 'just now';
  if (abs < 60) return rtf.format(s, 'second');
  if (abs < 3600) return rtf.format(Math.round(s / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(s / 3600), 'hour');
  return rtf.format(Math.round(s / 86400), 'day');
}

/** 1234 -> "1,234". */
export function count(n: number): string {
  return new Intl.NumberFormat('en').format(n);
}

/** 12 -> "12 ms", 1530 -> "1.5 s", 15000 -> "15 s". */
export function duration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = ms / 1000;
  return `${s < 10 ? s.toFixed(1) : Math.round(s)} s`;
}

/**
 * An endpoint URL for compact display: host and path, so two endpoints on one
 * host stay apart. The scheme, query and fragment are left out (a query may
 * carry a token). The URL itself if it does not parse.
 */
export function target(url: string): string {
  try {
    const u = new URL(url);
    const path = u.pathname.replace(/\/+$/, '');
    return u.host + path;
  } catch {
    return url;
  }
}

/** First 8 characters of an id. */
export function shortId(id: string): string {
  return id.slice(0, 8);
}

/** Share of finished deliveries that succeeded, or null when none finished. */
export function successRate(succeeded: number, dead: number): number | null {
  const finished = succeeded + dead;
  return finished === 0 ? null : (succeeded / finished) * 100;
}

/** "99.9%", "100%", "–" for no data. */
export function percent(p: number | null): string {
  if (p === null) return '–';
  if (p === 100 || p === 0) return `${p}%`;
  return `${Math.min(p, 99.9).toFixed(1)}%`;
}

/**
 * How an attempt is shown: the worker's own interruptions and lost leases
 * are told apart from the endpoint's failures, as the service records them.
 */
export function attemptKind(a: { status_code?: number | undefined; error?: string | undefined }): 'ok' | 'fail' | 'interrupted' | 'lost' {
  if (a.status_code !== undefined && a.status_code >= 200 && a.status_code < 300 && !a.error) return 'ok';
  if (a.error?.startsWith('canceled:')) return 'interrupted';
  if (a.error?.startsWith('lease expired')) return 'lost';
  return 'fail';
}

/** A random Idempotency-Key: 1 to 255 printable ASCII, no spaces. */
export function newIdempotencyKey(): string {
  return 'ui-' + crypto.randomUUID();
}
