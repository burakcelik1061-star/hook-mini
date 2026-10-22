import { useEffect, useRef, useState, type ReactNode } from 'react';
import { describeError } from '../api';
import type { DeliveryStatus } from '../types';

export function Logo({ size = 28 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 28 28" fill="none" aria-hidden="true">
      <path d="M14 2 25 8v12l-11 6-11-6V8l11-6Z" stroke="var(--accent)" strokeWidth="1.6" />
      <path d="M3 8l11 6 11-6M14 14v12" stroke="var(--accent)" strokeWidth="1.6" opacity=".55" />
    </svg>
  );
}

const statusTag: Record<DeliveryStatus, { cls: string; label: string }> = {
  succeeded: { cls: 't-ok', label: '✓ succeeded' },
  dead: { cls: 't-bad', label: '✕ dead' },
  pending: { cls: 't-warn', label: '◷ pending' },
  in_flight: { cls: 't-info', label: '↗ in flight' },
};

export function StatusTag({ status }: { status: DeliveryStatus }) {
  const t = statusTag[status];
  return <span className={`tag ${t.cls}`}>{t.label}</span>;
}

export function ErrorNotice({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  return (
    <div className="notice bad" role="alert">
      <span style={{ flex: 1 }}>{describeError(error)}</span>
      {onRetry && (
        <button className="btn btn-ghost" type="button" onClick={onRetry}>
          Try again
        </button>
      )}
    </div>
  );
}

export function Skeleton({ height = 18, width = '100%' }: { height?: number; width?: number | string }) {
  return <div className="skeleton" style={{ height, width }} aria-hidden="true" />;
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}

/**
 * Loads data now and then every intervalMs while the tab is visible (a
 * hidden tab spends none of the tenant's request budget). Errors are kept,
 * the last good data too.
 */
export function usePolling<T>(load: () => Promise<T>, intervalMs: number, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [tick, setTick] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const run = async () => {
      if (document.visibilityState === 'visible') {
        try {
          const d = await loadRef.current();
          if (alive) {
            setData(d);
            setError(null);
          }
        } catch (e) {
          if (alive) setError(e);
        }
      }
      if (alive && intervalMs > 0) timer = setTimeout(run, intervalMs);
    };
    void run();
    return () => {
      alive = false;
      if (timer) clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [intervalMs, tick, ...deps]);

  return { data, error, reload: () => setTick((t) => t + 1) };
}

/** Copies text; returns whether it worked, for a "Copied" confirmation. */
export async function copy(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}
