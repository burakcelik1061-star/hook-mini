import { target } from '../format';

export interface Target {
  id: string;
  url: string;
  active: boolean;
}

const DX = 456; // from the source's edge to the towers
const MID = 150; // the source's centre line on the 300 px plane
const SPREAD = 236; // vertical room for the towers

/** Where each target's tower sits and the beam that reaches it. */
export function layout(n: number): { y: number; length: number; angle: number }[] {
  return Array.from({ length: n }, (_, i) => {
    const y = n === 1 ? MID : 32 + (i * SPREAD) / (n - 1);
    const dy = y - MID;
    return { y, length: Math.hypot(DX, dy), angle: (Math.atan2(dy, DX) * 180) / Math.PI };
  });
}

/**
 * The send screen's 3D scene: one event fanning out to its endpoints. A
 * new sendKey fires the packets again; landed lights up the towers whose
 * delivery has succeeded. Disabled endpoints get a dashed beam and nothing.
 */
export function FanOut({ targets, sendKey, landed }: { targets: Target[]; sendKey: number; landed: Set<string> }) {
  const shown = targets.slice(0, 6);
  const pos = layout(shown.length);
  return (
    <div className="fan" role="img" aria-label={`One event fanning out to ${shown.filter((t) => t.active).length} active endpoints`}>
      <div className="fan-plane">
        <div className="src" style={{ top: MID - 28 }}>
          {[0, 8, 16, 24].map((z) => <div key={z} className="slab" style={{ transform: `translateZ(${z}px)` }} />)}
          <div className="slab top" style={{ transform: 'translateZ(32px)' }} />
        </div>
        {shown.map((t, i) => {
          const p = pos[i]!;
          return (
            <div key={t.id} className={`beam${t.active ? '' : ' off'}`} style={{ top: MID, width: p.length, transform: `rotate(${p.angle}deg)` }}>
              {t.active && sendKey > 0 && <span key={sendKey} className="pkt" style={{ animationDelay: `${i * 0.08}s` }} />}
            </div>
          );
        })}
        {shown.map((t, i) => {
          const p = pos[i]!;
          return (
            <div key={t.id} className={`tower${t.active ? '' : ' off'}${landed.has(t.id) ? ' landed' : ''}`} style={{ top: p.y - 28 }} title={target(t.url)}>
              <div className="slab" style={{ transform: 'translateZ(0px)' }} />
              <div className="slab" style={{ transform: 'translateZ(8px)' }} />
              {t.active && <div className="slab top" style={{ transform: 'translateZ(16px)' }} />}
            </div>
          );
        })}
      </div>
    </div>
  );
}
