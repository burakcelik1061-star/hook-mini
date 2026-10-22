import { useState, type FormEvent } from 'react';
import { ApiError, describeError } from '../api';
import { copy, Empty, ErrorNotice, Skeleton, usePolling } from '../components/ui';
import { percent, relativeTime, successRate } from '../format';
import { Link, paths } from '../router';
import { useSession } from '../session';
import type { Endpoint } from '../types';

/** A secret to show once: after creating an endpoint or rotating its secret. */
interface Revealed {
  endpoint: Endpoint;
  secret: string;
  rotated: boolean;
}

export function Endpoints() {
  const { client } = useSession();
  const list = usePolling(async () => {
    const all: Endpoint[] = [];
    let cursor: string | undefined;
    do {
      const page = await client.listEndpoints(cursor);
      all.push(...page.data);
      cursor = page.next_cursor ?? undefined;
    } while (cursor && all.length < 500);
    return all;
  }, 15000);
  const stats = usePolling(() => client.stats(), 15000);
  const [panel, setPanel] = useState<'closed' | 'form'>(() => 'closed');
  const [revealed, setRevealed] = useState<Revealed | null>(null);
  const [notice, setNotice] = useState<{ ok: boolean; text: string } | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);

  const byId = new Map(stats.data?.endpoints.map((e) => [e.id, e]) ?? []);

  async function act(label: string, fn: () => Promise<unknown>) {
    try {
      await fn();
      setNotice({ ok: true, text: label });
      list.reload();
      stats.reload();
    } catch (e) {
      setNotice({ ok: false, text: describeError(e) });
    }
  }

  async function rotate(ep: Endpoint) {
    try {
      const r = await client.rotateSecret(ep.id);
      if (r.secret) setRevealed({ endpoint: r, secret: r.secret, rotated: true });
      setNotice(null);
      list.reload();
    } catch (e) {
      setNotice({ ok: false, text: describeError(e) });
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>Endpoints</h1>
          <p>Where your events are delivered. Each has its own signing secret.</p>
        </div>
        <button className="btn btn-primary" type="button" onClick={() => { setPanel('form'); setRevealed(null); }}>
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M8 2v12M2 8h12" stroke="currentColor" strokeWidth="1.8" /></svg>
          Add endpoint
        </button>
      </div>
      {notice && (
        <div className={`notice ${notice.ok ? 'ok' : 'bad'}`} role="status">
          {notice.text}
        </div>
      )}

      <div className="split">
        <section className="main" style={{ display: 'flex', flexDirection: 'column', gap: 16 }} aria-label="Endpoints">
          {list.error !== null && !list.data && <ErrorNotice error={list.error} onRetry={list.reload} />}
          {!list.data && !list.error && [0, 1, 2].map((i) => <div key={i} className="card" style={{ padding: 22 }}><Skeleton height={60} /></div>)}
          {list.data?.length === 0 && (
            <div className="card">
              <Empty>No endpoints yet. Add the URL that should receive your webhooks.</Empty>
            </div>
          )}
          {list.data?.map((ep) => {
            const st = byId.get(ep.id);
            const rate = st ? successRate(st.last_24h.succeeded, st.last_24h.dead) : null;
            const off = ep.status === 'disabled';
            const grace = ep.previous_secret_expires_at && Date.parse(ep.previous_secret_expires_at) > Date.now();
            return (
              <article key={ep.id} className="card" style={{ padding: '20px 22px', display: 'flex', flexDirection: 'column', gap: 16 }}>
                <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 12 }}>
                  <div style={{ flex: '1 1 280px', minWidth: 0 }}>
                    <div className="mono ellipsis" style={{ fontSize: 15 }}>{ep.url}</div>
                    {ep.description && <div className="hint" style={{ marginTop: 4 }}>{ep.description}</div>}
                  </div>
                  <span className={`tag ${off ? 't-bad' : 't-ok'}`}>{off ? '✕ disabled' : '● active'}</span>
                </div>
                {off && (
                  <div style={{ fontSize: 13, color: 'var(--bad)', background: 'rgba(255,122,110,0.06)', borderRadius: 12, padding: '10px 14px' }}>
                    {ep.disabled_reason ?? 'Disabled.'} New events are not delivered to it; enable it, then replay its dead deliveries.
                  </div>
                )}
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 12 }}>
                  <Fig label="Success · 24 h" value={percent(rate)} />
                  <Fig label="Failures in a row" value={String(ep.consecutive_failures)} />
                  <Fig label="In flight now" value={st ? String(st.last_24h.in_flight) : '–'} />
                  <div>
                    <div className="kpi-label" style={{ fontSize: 12 }}>Signing secret</div>
                    <div style={{ fontSize: 13, marginTop: 6, color: 'var(--text-2)' }}>
                      {grace ? `Rotated · old one valid ${relativeTime(ep.previous_secret_expires_at).replace('in ', 'for ')}` : `Since ${relativeTime(ep.created_at)}`}
                    </div>
                  </div>
                </div>
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, borderTop: '1px solid var(--line)', paddingTop: 14 }}>
                  <button className="btn btn-ghost" type="button" onClick={() => rotate(ep)}>Rotate secret</button>
                  {off ? (
                    <button className="btn btn-ghost" type="button" onClick={() => act('Endpoint enabled.', () => client.enableEndpoint(ep.id))}>Enable</button>
                  ) : (
                    <button className="btn btn-ghost" type="button" onClick={() => act('Endpoint disabled. Deliveries that come due end as dead until you enable it.', () => client.disableEndpoint(ep.id))}>Disable</button>
                  )}
                  <Link to={paths.deliveries} query={`?endpoint=${ep.id}`} className="btn btn-ghost">Deliveries</Link>
                  <span style={{ marginLeft: 'auto', display: 'flex', gap: 10 }}>
                    {confirmDelete === ep.id ? (
                      <>
                        <button className="btn btn-danger" type="button" onClick={() => { setConfirmDelete(null); void act('Endpoint deleted, with its delivery history.', () => client.deleteEndpoint(ep.id)); }}>
                          Delete with its history
                        </button>
                        <button className="btn btn-ghost" type="button" onClick={() => setConfirmDelete(null)}>Keep</button>
                      </>
                    ) : (
                      <button className="btn btn-danger" type="button" onClick={() => setConfirmDelete(ep.id)}>Delete</button>
                    )}
                  </span>
                </div>
              </article>
            );
          })}
        </section>

        {(panel === 'form' || revealed) && (
          <aside className="side card" aria-label={revealed ? 'Signing secret' : 'Add endpoint'} style={{ padding: 24, display: 'flex', flexDirection: 'column', gap: 20 }}>
            {revealed ? (
              <SecretOnce revealed={revealed} onDone={() => { setRevealed(null); setPanel('closed'); }} />
            ) : (
              <CreateForm
                onCancel={() => setPanel('closed')}
                onCreated={(ep) => {
                  if (ep.secret) setRevealed({ endpoint: ep, secret: ep.secret, rotated: false });
                  list.reload();
                  stats.reload();
                }}
              />
            )}
          </aside>
        )}
      </div>
    </div>
  );
}

function Fig({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="kpi-label" style={{ fontSize: 12 }}>{label}</div>
      <div className="mono" style={{ fontSize: 18, marginTop: 4 }}>{value}</div>
    </div>
  );
}

function CreateForm({ onCancel, onCreated }: { onCancel: () => void; onCreated: (e: Endpoint) => void }) {
  const { client } = useSession();
  const [url, setUrl] = useState('');
  const [description, setDescription] = useState('');
  const [error, setError] = useState<{ field?: string | undefined; text: string } | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      onCreated(await client.createEndpoint(url.trim(), description.trim()));
    } catch (err) {
      setError({ field: err instanceof ApiError ? err.field : undefined, text: describeError(err) });
    } finally {
      setBusy(false);
    }
  }

  const urlError = error && (error.field === 'url' || error.field === 'body') ? error.text : null;
  return (
    <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <h2 style={{ margin: 0, fontSize: 20, fontWeight: 600 }}>Add endpoint</h2>
        <button className="btn btn-ghost" type="button" aria-label="Close" onClick={onCancel} style={{ padding: '0 12px' }}>
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M3 3l10 10M13 3 3 13" stroke="currentColor" strokeWidth="1.8" /></svg>
        </button>
      </div>
      <div className="field">
        <label htmlFor="ep-url">URL</label>
        <input id="ep-url" className="input mono" type="url" required placeholder="https://example.com/webhooks" value={url} onChange={(e) => setUrl(e.target.value)} aria-invalid={urlError ? 'true' : undefined} aria-describedby="ep-url-hint" />
        <div id="ep-url-hint" className="hint">HTTP or HTTPS. Private, loopback and cloud metadata addresses are refused, here and again on every connection.</div>
        {urlError && <div className="field-error" role="alert">{urlError}</div>}
      </div>
      <div className="field">
        <label htmlFor="ep-desc">
          Description <span className="muted" style={{ fontWeight: 400 }}>(optional)</span>
        </label>
        <input id="ep-desc" className="input" type="text" maxLength={500} value={description} onChange={(e) => setDescription(e.target.value)} />
      </div>
      {error && !urlError && <div className="field-error" role="alert">{error.text}</div>}
      <button className="btn btn-primary" type="submit" disabled={busy || url.trim() === ''}>
        {busy ? 'Creating…' : 'Create endpoint'}
      </button>
    </form>
  );
}

function SecretOnce({ revealed, onDone }: { revealed: Revealed; onDone: () => void }) {
  const [copied, setCopied] = useState(false);
  return (
    <>
      <h2 style={{ margin: 0, fontSize: 20, fontWeight: 600 }}>{revealed.rotated ? 'New signing secret' : 'Endpoint created'}</h2>
      <p style={{ margin: 0, fontSize: 14, lineHeight: 1.6, color: 'var(--text-2)' }}>
        Give this secret to the service behind <span className="mono">{revealed.endpoint.url}</span> so it can verify signatures. <strong style={{ color: 'var(--warn)' }}>It is shown only now:</strong> Hook Mini stores it encrypted and cannot show it again.
        {revealed.rotated && ' For 24 hours requests are signed with both the old and the new secret, so the receiver can switch over without rejecting anything.'}
      </p>
      <div className="vault">
        <div className="vault-card">
          <div style={{ fontSize: 11, letterSpacing: '.08em', textTransform: 'uppercase', color: 'var(--ok)' }}>Signing secret</div>
          <div className="mono" style={{ fontSize: 14, marginTop: 10, wordBreak: 'break-all', lineHeight: 1.6 }}>{revealed.secret}</div>
        </div>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10 }}>
        <button className="btn btn-primary" type="button" onClick={async () => setCopied(await copy(revealed.secret))}>
          {copied ? 'Copied' : 'Copy secret'}
        </button>
        <button className="btn btn-ghost" type="button" onClick={onDone}>I have stored it</button>
      </div>
      <div className="hint">Verify with any Standard Webhooks library, or <span className="mono">pkg/webhook</span> in Go.</div>
    </>
  );
}
