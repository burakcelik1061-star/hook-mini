import { useEffect, useRef, useState } from 'react';
import { target } from '../format';
import type { Delivery, Stats } from '../types';

type Kind = 'ok' | 'retry' | 'dead';
interface Packet {
  key: number;
  lane: string;
  kind: Kind;
}

const MAX_LANES = 6;
const MAX_PER_LANE = 6;

/**
 * What changed between two polls, as packets: a delivery that has just
 * succeeded, just died, or just failed an attempt and is waiting to retry.
 * The first poll is the baseline and produces nothing, so opening the page
 * does not replay history as if it were live.
 */
export function changes(prev: Map<string, Delivery> | null, next: Delivery[]): { endpointId: string; kind: Kind }[] {
  if (!prev) return [];
  const out: { endpointId: string; kind: Kind }[] = [];
  for (const d of next) {
    const before = prev.get(d.id);
    if (before && before.status === d.status && before.attempt_count === d.attempt_count) continue;
    if (d.status === 'succeeded') out.push({ endpointId: d.endpoint_id, kind: 'ok' });
    else if (d.status === 'dead') out.push({ endpointId: d.endpoint_id, kind: 'dead' });
    else if (d.status === 'pending' && d.attempt_count > 0 && (!before || d.attempt_count > before.attempt_count)) out.push({ endpointId: d.endpoint_id, kind: 'retry' });
  }
  return out;
}

/** The overview's 3D scene: the queue, one lane per endpoint, live packets. */
export function Pipeline({ endpoints, recent }: { endpoints: Stats['endpoints']; recent: Delivery[] | null }) {
  const [packets, setPackets] = useState<Packet[]>([]);
  const prev = useRef<Map<string, Delivery> | null>(null);
  const seq = useRef(0);
  // Each batch of packets is removed by its own timer. They are not cleared
  // when the next poll arrives (every 2 s, sooner than a packet's 6.5 s), only
  // when the scene goes away.
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  useEffect(() => {
    const all = timers.current;
    return () => all.forEach(clearTimeout);
  }, []);

  const lanes = [...endpoints].sort((a, b) => (a.status === b.status ? 0 : a.status === 'active' ? -1 : 1)).slice(0, MAX_LANES);
  const laneIds = new Set(lanes.map((l) => l.id));

  useEffect(() => {
    if (!recent) return;
    const fresh = changes(prev.current, recent).filter((c) => laneIds.has(c.endpointId));
    prev.current = new Map(recent.map((d) => [d.id, d]));
    if (fresh.length === 0) return;
    const added = fresh.map((c) => ({ key: ++seq.current, lane: c.endpointId, kind: c.kind }));
    setPackets((p) => {
      const all = [...p, ...added];
      // Bound the scene: at most a few packets per lane.
      const perLane = new Map<string, number>();
      return all.reverse().filter((x) => {
        const n = (perLane.get(x.lane) ?? 0) + 1;
        perLane.set(x.lane, n);
        return n <= MAX_PER_LANE;
      }).reverse();
    });
    const keys = new Set(added.map((a) => a.key));
    const t = setTimeout(() => {
      timers.current.delete(t);
      setPackets((p) => p.filter((x) => !keys.has(x.key)));
    }, 6500);
    timers.current.add(t);
    // laneIds is derived from endpoints, which the parent refreshes with recent
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [recent]);

  const label = lanes.length
    ? `Deliveries flowing from the Postgres queue to ${lanes.length} endpoints`
    : 'No endpoints yet';

  return (
    <div className="scene" role="img" aria-label={label}>
      <div className="plane">
        <div className="queue">
          {[0, 8, 16, 24, 32, 40].map((z) => (
            <div key={z} className="slab" style={{ transform: `translateZ(${z}px)` }} />
          ))}
          <div className="slab top" style={{ transform: 'translateZ(48px)' }} />
        </div>
        <div className="lanes">
          {lanes.map((l) => (
            <div key={l.id} className={`lane${l.status === 'disabled' ? ' off' : ''}`}>
              <span className="lane-label">{target(l.url)}</span>
              {packets
                .filter((p) => p.lane === l.id)
                .map((p) => (
                  <span key={p.key} className={`pkt ${p.kind}`} />
                ))}
              <span className="receiver" />
            </div>
          ))}
        </div>
      </div>
      <div className="scene-caption">
        Postgres queue
        <br />
        <span style={{ color: 'var(--text)' }}>FOR UPDATE SKIP LOCKED</span>
      </div>
    </div>
  );
}
