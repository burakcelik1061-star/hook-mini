import { describe, expect, it } from 'vitest';
import { API_KEY_PATTERN, createKeyStore, keyHint, storageOrMemory, type StorageLike } from './auth';
import { layout } from './components/FanOut';
import { changes } from './components/Pipeline';
import { attemptKind, count, duration, percent, relativeTime, successRate, target } from './format';
import type { Delivery } from './types';

function memory(): StorageLike & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return { data, getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v), removeItem: (k) => void data.delete(k) };
}

describe('the key store', () => {
  const KEY = 'hm_0123abcd_' + 'b'.repeat(64);

  it('keeps the key for the tab only, unless asked to remember it', () => {
    const session = memory(), local = memory();
    const s = createKeyStore(session, local);
    s.set(KEY, false);
    expect(session.data.size).toBe(1);
    expect(local.data.size).toBe(0);
    expect(s.get()).toBe(KEY);

    s.set(KEY, true);
    expect(session.data.size).toBe(0);
    expect(local.data.size).toBe(1);
    expect(s.get()).toBe(KEY);
  });

  it('keeps exactly one copy when the choice changes either way', () => {
    const session = memory(), local = memory();
    const s = createKeyStore(session, local);
    s.set(KEY, true);
    s.set(KEY, false); // no longer remembered: the copy on the device must go
    expect(local.data.size).toBe(0);
    expect(session.data.size).toBe(1);
  });

  it('forgets it everywhere on sign-out', () => {
    const session = memory(), local = memory();
    const s = createKeyStore(session, local);
    s.set(KEY, true);
    session.setItem('hookmini.apiKey', KEY); // even a stray second copy
    s.clear();
    expect(s.get()).toBeNull();
    expect(session.data.size + local.data.size).toBe(0);
  });

  it('accepts exactly the shape of an API key', () => {
    expect(API_KEY_PATTERN.test(KEY)).toBe(true);
    for (const bad of ['', 'hm_0123abcd', KEY + 'a', KEY.toUpperCase(), ' ' + KEY, 'hm_0123abcg_' + 'b'.repeat(64)]) {
      expect(API_KEY_PATTERN.test(bad)).toBe(false);
    }
  });

  it('still works when the browser refuses storage', () => {
    const blocked = () => {
      throw new DOMException('The operation is insecure.', 'SecurityError');
    };
    const full: StorageLike = { getItem: () => null, setItem: () => { throw new DOMException('quota', 'QuotaExceededError'); }, removeItem: () => {} };
    for (const get of [blocked, () => full]) {
      const s = createKeyStore(storageOrMemory(get), storageOrMemory(get));
      s.set(KEY, false);
      expect(s.get()).toBe(KEY);
      s.clear();
      expect(s.get()).toBeNull();
    }
    const real = memory();
    expect(storageOrMemory(() => real)).toBe(real); // a working storage is used as it is
    expect(real.data.size).toBe(0); // and the probe leaves nothing behind
  });

  it('shows only a prefix of the key', () => {
    expect(keyHint(KEY)).toBe('hm_0123abcd…');
    expect(keyHint(null)).toBe('');
  });
});

describe('formatting', () => {
  const now = Date.parse('2026-10-22T12:00:00Z');
  it('relative time', () => {
    expect(relativeTime('2026-10-22T11:59:55Z', now)).toBe('just now');
    expect(relativeTime('2026-10-22T11:57:00Z', now)).toBe('3 minutes ago');
    expect(relativeTime('2026-10-22T12:07:00Z', now)).toBe('in 7 minutes');
    expect(relativeTime('2026-10-22T10:00:00Z', now)).toBe('2 hours ago');
    expect(relativeTime('2026-10-18T12:00:00Z', now)).toBe('4 days ago');
    expect(relativeTime(undefined, now)).toBe('');
    expect(relativeTime('not a date', now)).toBe('');
  });

  it('numbers, durations, endpoint targets', () => {
    expect(count(182406)).toBe('182,406');
    expect(duration(64)).toBe('64 ms');
    expect(duration(1530)).toBe('1.5 s');
    expect(duration(15000)).toBe('15 s');
    expect(target('https://billing.example.com:8443/hooks/?token=s3cret#x')).toBe('billing.example.com:8443/hooks');
    expect(target('https://billing.example.com/')).toBe('billing.example.com');
    expect(target('http://receiver:9000/status/503')).toBe('receiver:9000/status/503');
    expect(target('not a url')).toBe('not a url');
  });

  it('success rate is over finished deliveries and never rounds up to 100%', () => {
    expect(successRate(0, 0)).toBeNull();
    expect(percent(successRate(0, 0))).toBe('–');
    expect(percent(successRate(5, 0))).toBe('100%');
    expect(percent(successRate(0, 5))).toBe('0%');
    expect(percent(successRate(9999, 1))).toBe('99.9%'); // 99.99 must not show as 100%
  });

  it('tells the worker interruptions and lost leases from endpoint failures', () => {
    expect(attemptKind({ status_code: 200 })).toBe('ok');
    expect(attemptKind({ status_code: 503 })).toBe('fail');
    expect(attemptKind({ error: 'canceled: the worker stopped before the endpoint responded' })).toBe('interrupted');
    expect(attemptKind({ error: 'lease expired: the worker stopped before reporting a result' })).toBe('lost');
    expect(attemptKind({ error: 'dns: no such host' })).toBe('fail');
  });
});

describe('the live pipeline', () => {
  const d = (id: string, status: Delivery['status'], attempts: number, endpoint = 'e1'): Delivery => ({ id, event_id: 'ev', endpoint_id: endpoint, status, attempt_count: attempts, created_at: '' });

  it('treats the first poll as the baseline: no history replayed as live', () => {
    expect(changes(null, [d('a', 'succeeded', 1)])).toEqual([]);
  });

  it('turns transitions into packets, and nothing else', () => {
    const before = new Map([
      ['a', d('a', 'in_flight', 0)],
      ['b', d('b', 'in_flight', 0)],
      ['c', d('c', 'pending', 1)],
      ['s', d('s', 'succeeded', 1)],
    ]);
    const after = [
      d('a', 'succeeded', 1, 'e1'), // delivered
      d('b', 'dead', 3, 'e2'), // died
      d('c', 'pending', 2, 'e3'), // failed again, will retry
      d('s', 'succeeded', 1), // unchanged
      d('n', 'pending', 0), // new, not attempted yet
    ];
    expect(changes(before, after)).toEqual([
      { endpointId: 'e1', kind: 'ok' },
      { endpointId: 'e2', kind: 'dead' },
      { endpointId: 'e3', kind: 'retry' },
    ]);
  });

  it('does not show an interrupted attempt as a failure', () => {
    // The worker stopped mid-attempt: the delivery goes back to pending
    // without the attempt being counted. Nothing failed at the endpoint.
    const before = new Map([['a', d('a', 'in_flight', 1)]]);
    expect(changes(before, [d('a', 'pending', 1)])).toEqual([]);
  });
});

describe('the fan-out geometry', () => {
  it('reaches every tower from the source', () => {
    expect(layout(1)).toEqual([{ y: 150, length: 456, angle: 0 }]);
    const five = layout(5);
    expect(five.map((p) => Math.round(p.y))).toEqual([32, 91, 150, 209, 268]);
    expect(five[2]!.angle).toBe(0);
    expect(five[0]!.angle).toBeCloseTo(-five[4]!.angle, 6); // symmetric
    expect(five[0]!.length).toBeCloseTo(Math.hypot(456, 118), 6);
  });
});
