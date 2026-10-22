import { useEffect, useState, type FormEvent } from 'react';
import { ApiError, describeError } from '../api';
import { FanOut } from '../components/FanOut';
import { StatusTag, usePolling } from '../components/ui';
import { target, newIdempotencyKey, shortId } from '../format';
import { Link, paths } from '../router';
import { useSession } from '../session';
import type { Delivery } from '../types';

const EXAMPLE = `{
  "invoice": "inv_1001",
  "amount": 4200,
  "currency": "EUR"
}`;

interface Sent {
  eventId: string;
  status: number;
  replayed: boolean;
  deliveryCount: number | undefined;
  at: number;
}

export function SendEvent() {
  const { client } = useSession();
  const endpoints = usePolling(async () => (await client.listEndpoints()).data, 30000);
  const [type, setType] = useState('invoice.paid');
  const [payload, setPayload] = useState(EXAMPLE);
  const [idemKey, setIdemKey] = useState('');
  const [errors, setErrors] = useState<{ type?: string; payload?: string; key?: string; form?: string }>({});
  const [busy, setBusy] = useState(false);
  const [sent, setSent] = useState<Sent | null>(null);
  const [sendKey, setSendKey] = useState(0);
  const [results, setResults] = useState<Delivery[]>([]);

  // Follow the event's deliveries for a minute after sending.
  useEffect(() => {
    if (!sent) return;
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      try {
        const page = await client.listDeliveries({ eventId: sent.eventId, limit: 100 });
        if (alive) setResults(page.data);
        const open = page.data.some((d) => d.status === 'pending' || d.status === 'in_flight');
        if (alive && open && Date.now() - sent.at < 60_000) timer = setTimeout(poll, 1000);
      } catch {
        if (alive) timer = setTimeout(poll, 3000);
      }
    };
    void poll();
    return () => {
      alive = false;
      if (timer) clearTimeout(timer);
    };
  }, [client, sent]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const next: typeof errors = {};
    let body: unknown;
    try {
      body = JSON.parse(payload);
      if (body === null || typeof body !== 'object' || Array.isArray(body)) next.payload = 'The payload must be a JSON object, like {"amount": 42}.';
    } catch {
      next.payload = 'This is not valid JSON.';
    }
    if (!/^[a-z0-9]+([._-][a-z0-9]+)*$/.test(type.trim())) next.type = 'Lower-case words joined by ".", "_" or "-", like invoice.paid.';
    if (idemKey && !/^[\x21-\x7e]{1,255}$/.test(idemKey)) next.key = '1 to 255 printable characters, no spaces.';
    setErrors(next);
    if (Object.keys(next).length) return;

    setBusy(true);
    try {
      const r = await client.createEvent(type.trim(), body, idemKey || undefined);
      setSent({ eventId: r.event.id, status: r.status, replayed: r.replayed, deliveryCount: r.event.delivery_count, at: Date.now() });
      setResults([]);
      if (!r.replayed) setSendKey((k) => k + 1);
    } catch (err) {
      const field = err instanceof ApiError ? err.field : undefined;
      const text = describeError(err);
      setErrors(field === 'type' ? { type: text } : field === 'payload' ? { payload: text } : field === 'Idempotency-Key' ? { key: text } : { form: text });
    } finally {
      setBusy(false);
    }
  }

  const eps = endpoints.data ?? [];
  const hostOf = new Map(eps.map((e) => [e.id, target(e.url)]));
  const active = eps.filter((e) => e.status === 'active').length;
  const landed = new Set(results.filter((d) => d.status === 'succeeded').map((d) => d.endpoint_id));

  return (
    <div className="page">
      <div className="split">
        <section className="side card" aria-label="Compose an event" style={{ padding: 24 }}>
          <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 18 }}>
            <div>
              <h1 style={{ margin: 0, fontSize: 32, letterSpacing: '-0.03em', fontWeight: 700 }}>Send event</h1>
              <p style={{ margin: '8px 0 0', color: 'var(--muted)', fontSize: 15, lineHeight: 1.6 }}>Every active endpoint gets its own delivery, signed and retried on its own.</p>
            </div>
            <div className="field">
              <label htmlFor="ev-type">Type</label>
              <input id="ev-type" className="input mono" value={type} onChange={(e) => setType(e.target.value)} aria-invalid={errors.type ? 'true' : undefined} />
              {errors.type && <div className="field-error" role="alert">{errors.type}</div>}
            </div>
            <div className="field">
              <label htmlFor="ev-payload">
                Payload <span className="muted" style={{ fontWeight: 400 }}>(JSON object, up to 256 KiB)</span>
              </label>
              <textarea id="ev-payload" className="input mono" rows={8} value={payload} onChange={(e) => setPayload(e.target.value)} spellCheck={false} aria-invalid={errors.payload ? 'true' : undefined} />
              {errors.payload && <div className="field-error" role="alert">{errors.payload}</div>}
            </div>
            <div className="field">
              <label htmlFor="ev-key">
                Idempotency-Key <span className="muted" style={{ fontWeight: 400 }}>(optional)</span>
              </label>
              <div style={{ display: 'flex', gap: 10 }}>
                <input id="ev-key" className="input mono" value={idemKey} onChange={(e) => setIdemKey(e.target.value)} placeholder="order-1001" aria-invalid={errors.key ? 'true' : undefined} aria-describedby="ev-key-hint" />
                <button className="btn btn-ghost" type="button" onClick={() => setIdemKey(newIdempotencyKey())}>Generate</button>
              </div>
              <div id="ev-key-hint" className="hint">Send twice with the same key and the same body: you get the first event back and nothing is delivered twice.</div>
              {errors.key && <div className="field-error" role="alert">{errors.key}</div>}
            </div>
            {errors.form && <div className="notice bad" role="alert">{errors.form}</div>}
            <button className="btn btn-primary" type="submit" disabled={busy}>
              {busy ? 'Sending…' : sent && idemKey ? 'Send again with the same key' : 'Send event'}
            </button>
            <details style={{ fontSize: 13, color: 'var(--muted)' }}>
              <summary style={{ cursor: 'pointer', minHeight: 32, display: 'flex', alignItems: 'center' }}>As a curl command</summary>
              <div className="code" style={{ marginTop: 8 }}>{`curl -X POST ${window.location.origin}/v1/events \\
  -H "Authorization: Bearer $KEY" \\
  -H "Content-Type: application/json" \\${idemKey ? `\n  -H "Idempotency-Key: ${idemKey}" \\` : ''}
  -d '{"type":"${type}","payload":${payload.replace(/\s+/g, ' ').trim()}}'`}</div>
            </details>
          </form>
        </section>

        <section className="main" style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
          <figure className="card" style={{ margin: 0, padding: '20px 22px 6px', overflow: 'hidden' }}>
            <figcaption style={{ display: 'flex', flexWrap: 'wrap', justifyContent: 'space-between', gap: 10 }}>
              <span style={{ fontSize: 14, fontWeight: 600 }}>Fan-out</span>
              <span className="muted" style={{ fontSize: 13 }}>
                {active} active endpoint{active === 1 ? '' : 's'}
                {eps.length > active ? ` · ${eps.length - active} disabled` : ''}
              </span>
            </figcaption>
            {eps.length === 0 ? (
              <div className="empty">
                No endpoints yet: the event would be stored with no deliveries. <Link to={paths.endpoints}>Add an endpoint</Link> first.
              </div>
            ) : (
              <FanOut targets={eps.map((e) => ({ id: e.id, url: e.url, active: e.status === 'active' }))} sendKey={sendKey} landed={landed} />
            )}
          </figure>

          <div className="card" style={{ overflow: 'hidden' }} aria-live="polite">
            <div className="card-head">
              <h2>Response</h2>
              {sent && (
                <span className={`tag mono ${sent.replayed ? 't-info' : 't-ok'}`}>
                  {sent.replayed ? `${sent.status} · Idempotent-Replayed: true` : `${sent.status} Created · delivery_count ${sent.deliveryCount ?? 0}`}
                </span>
              )}
            </div>
            {!sent && <div className="empty">Nothing sent yet. The answer and each delivery appear here.</div>}
            {sent && (
              <>
                <div style={{ padding: '0 20px 14px', fontSize: 13, color: 'var(--text-2)', lineHeight: 1.6 }}>
                  {sent.replayed
                    ? `Same key, same body: this is event ${shortId(sent.eventId)} from the earlier request. No new deliveries were created.`
                    : `Event ${shortId(sent.eventId)} was stored together with its ${sent.deliveryCount ?? 0} deliveries in one transaction.`}
                </div>
                {results.map((d) => (
                  <div className="row" key={d.id}>
                    <span className="mono ellipsis" style={{ flex: '1 1 220px', minWidth: 0, fontSize: 13 }}>{hostOf.get(d.endpoint_id) ?? shortId(d.endpoint_id)}</span>
                    <StatusTag status={d.status} />
                    <span className="mono muted" style={{ flex: '0 0 180px', textAlign: 'right', fontSize: 13 }}>
                      {d.last_status_code ? `HTTP ${d.last_status_code}` : d.last_error ? 'no response' : ''}
                      {d.attempt_count ? ` · attempt ${d.attempt_count}` : ''}
                    </span>
                  </div>
                ))}
                <div style={{ padding: '14px 20px', borderTop: '1px solid var(--line)' }}>
                  <Link to={paths.deliveries} style={{ fontSize: 14, minHeight: 44, display: 'inline-flex', alignItems: 'center' }}>Follow them in Deliveries →</Link>
                </div>
              </>
            )}
          </div>
        </section>
      </div>
    </div>
  );
}
