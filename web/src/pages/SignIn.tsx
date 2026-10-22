import { useState, type FormEvent, type MouseEvent, type ReactNode } from 'react';
import { ApiError, describeError } from '../api';
import { API_KEY_PATTERN } from '../auth';
import { Logo } from '../components/ui';
import { navigate, paths } from '../router';
import { clientForKey, useSession } from '../session';

export function SignIn() {
  const { signIn } = useSession();
  const [key, setKey] = useState('');
  const [remember, setRemember] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [tilt, setTilt] = useState({ rx: 10, ry: -14 });

  async function submit(e: FormEvent) {
    e.preventDefault();
    const k = key.trim();
    if (!API_KEY_PATTERN.test(k)) {
      setError('That does not look like a Hook Mini API key (hm_ followed by 8 and 64 hex characters).');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      // Prove the key works before keeping it.
      await clientForKey(k).stats();
      signIn(k, remember);
      navigate(paths.overview);
    } catch (err) {
      setError(err instanceof ApiError && err.status === 401 ? 'That key was not accepted. It may have been revoked.' : describeError(err));
    } finally {
      setBusy(false);
    }
  }

  function onMove(e: MouseEvent<HTMLElement>) {
    const r = e.currentTarget.getBoundingClientRect();
    const x = (e.clientX - r.left) / r.width - 0.5;
    const y = (e.clientY - r.top) / r.height - 0.5;
    setTilt({ rx: Math.round(10 - y * 14), ry: Math.round(-14 + x * 22) });
  }

  return (
    <div className="shell" style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'stretch' }}>
      <section onMouseMove={onMove} onMouseLeave={() => setTilt({ rx: 10, ry: -14 })} style={{ flex: '1.2 1 520px', minWidth: 0, padding: '48px 56px', display: 'flex', flexDirection: 'column', justifyContent: 'center', gap: 28, borderRight: '1px solid var(--line)' }}>
        <div className="brand">
          <Logo size={30} />
          Hook Mini
        </div>
        <div className="stage" aria-hidden="true" style={{ perspective: 1200, position: 'relative', height: 460 }}>
          <div style={{ position: 'absolute', inset: 0, transformStyle: 'preserve-3d', transition: 'transform .25s ease-out', transform: `rotateX(${tilt.rx}deg) rotateY(${tilt.ry}deg)` }}>
            <Layer z="translate3d(-70px, -150px, -120px) rotateZ(-4deg)" opacity={0.6} k="1 · Event accepted">
              POST /v1/events
              <br />
              <span className="muted">Idempotency-Key: order-1001</span>
            </Layer>
            <Layer z="translate3d(10px, -40px, -40px) rotateZ(1deg)" opacity={0.85} k="2 · Signed">
              webhook-signature
              <br />
              <span style={{ color: 'var(--accent)' }}>v1,k5oZfz0yr3R8…</span>
            </Layer>
            <Layer z="translate3d(70px, 70px, 40px) rotateZ(3deg)" opacity={1} k="3 · Delivered" highlight>
              200 OK · attempt 1
              <br />
              <span className="muted">signature verified by the receiver</span>
            </Layer>
          </div>
        </div>
        <p style={{ margin: 0, fontSize: 15, lineHeight: 1.6, color: 'var(--muted)', maxWidth: '52ch' }}>Webhooks that are signed, retried and accounted for, end to end.</p>
      </section>

      <section style={{ flex: '1 1 420px', minWidth: 0, padding: '48px 56px', display: 'flex', flexDirection: 'column', justifyContent: 'center' }}>
        <form onSubmit={submit} noValidate style={{ width: '100%', maxWidth: 420, display: 'flex', flexDirection: 'column', gap: 22 }}>
          <div>
            <h1 style={{ margin: 0, fontSize: 34, letterSpacing: '-0.03em', fontWeight: 700 }}>Sign in</h1>
            <p style={{ margin: '10px 0 0', fontSize: 15, lineHeight: 1.6, color: 'var(--muted)' }}>Use your tenant's API key. It stays in this browser and goes only to this Hook Mini API.</p>
          </div>
          <div className="field">
            <label htmlFor="key">API key</label>
            <input id="key" className="input mono" type="password" autoComplete="off" spellCheck={false} placeholder="hm_xxxxxxxx_…" value={key} onChange={(e) => setKey(e.target.value)} aria-invalid={error ? 'true' : undefined} aria-describedby={error ? 'key-error' : undefined} />
            {error && (
              <div id="key-error" className="field-error" role="alert">
                {error}
              </div>
            )}
          </div>
          <label style={{ display: 'flex', alignItems: 'flex-start', gap: 10, fontSize: 14, color: 'var(--text-2)', minHeight: 44 }}>
            <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} style={{ width: 20, height: 20, marginTop: 1, accentColor: 'var(--accent)' }} />
            <span>
              Remember this key on this device
              <span className="hint" style={{ display: 'block' }}>
                Off: it is forgotten when you close the tab. On: anyone using this browser profile can use it.
              </span>
            </span>
          </label>
          <button className="btn btn-primary" type="submit" disabled={busy} style={{ minHeight: 48 }}>
            {busy ? 'Checking the key…' : 'Connect'}
          </button>
          <div style={{ borderTop: '1px solid var(--line)', paddingTop: 18, fontSize: 13, lineHeight: 1.6, color: 'var(--muted)' }}>
            No key yet? An operator creates tenants and their first key:
            <div className="code" style={{ marginTop: 8 }}>
              docker compose run --rm api admin bootstrap "Acme Inc"
            </div>
          </div>
        </form>
      </section>
    </div>
  );
}

function Layer({ z, opacity, k, highlight, children }: { z: string; opacity: number; k: string; highlight?: boolean; children: ReactNode }) {
  return (
    <div style={{ position: 'absolute', left: '50%', top: '50%', width: 380, marginLeft: -190, borderRadius: 20, padding: '20px 22px', background: 'linear-gradient(180deg,#121821,#0d1218)', border: `1px solid ${highlight ? 'rgba(110,242,192,.35)' : 'rgba(255,255,255,.08)'}`, boxShadow: '0 30px 80px rgba(0,0,0,.5)', transform: z, opacity }}>
      <div style={{ fontSize: 11, letterSpacing: '.08em', textTransform: 'uppercase', color: 'var(--muted)' }}>{k}</div>
      <div className="mono" style={{ fontSize: 13, marginTop: 8, lineHeight: 1.6, wordBreak: 'break-all' }}>
        {children}
      </div>
    </div>
  );
}
