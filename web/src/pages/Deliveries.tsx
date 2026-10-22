import { useEffect, useRef, useState } from 'react';
import { ApiError, describeError } from '../api';
import { Empty, ErrorNotice, Skeleton, StatusTag, usePolling } from '../components/ui';
import { attemptKind, duration, target, relativeTime, shortId } from '../format';
import { useSession } from '../session';
import type { Delivery, DeliveryStatus, Endpoint, Event } from '../types';

const filters: { id: DeliveryStatus | 'all'; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'pending', label: 'Pending' },
  { id: 'in_flight', label: 'In flight' },
  { id: 'succeeded', label: 'Succeeded' },
  { id: 'dead', label: 'Dead' },
];

function fromQuery(): { status: DeliveryStatus | 'all'; endpoint: string } {
  const q = new URLSearchParams(window.location.search);
  const s = q.get('status');
  return {
    status: filters.some((f) => f.id === s) ? (s as DeliveryStatus) : 'all',
    endpoint: q.get('endpoint') ?? '',
  };
}

export function Deliveries() {
  const { client } = useSession();
  const [status, setStatus] = useState<DeliveryStatus | 'all'>(() => fromQuery().status);
  const [endpoint, setEndpoint] = useState(() => fromQuery().endpoint);
  const [more, setMore] = useState<{ rows: Delivery[]; cursor: string | null }>({ rows: [], cursor: null });
  const [selected, setSelected] = useState<string | null>(null);
  const [notice, setNotice] = useState<{ ok: boolean; text: string } | null>(null);
  const types = useRef(new Map<string, string>());

  const endpoints = usePolling(async () => (await client.listEndpoints()).data, 30000);
  const first = usePolling(
    async () => {
      const page = await client.listDeliveries({ status: status === 'all' ? undefined : status, endpointId: endpoint || undefined, limit: 50 });
      await rememberTypes(page.data);
      return page;
    },
    5000,
    [status, endpoint],
  );

  // A link can change the query while this page stays on screen (the menu's
  // "Deliveries" from a filtered view, or back and forward): follow it, so
  // the view always matches the address.
  useEffect(() => {
    const follow = () => {
      const q = fromQuery();
      setStatus(q.status);
      setEndpoint(q.endpoint);
    };
    window.addEventListener('popstate', follow);
    return () => window.removeEventListener('popstate', follow);
  }, []);

  // A new filter starts over.
  useEffect(() => {
    setMore({ rows: [], cursor: null });
    const q = new URLSearchParams();
    if (status !== 'all') q.set('status', status);
    if (endpoint) q.set('endpoint', endpoint);
    window.history.replaceState(null, '', '/deliveries' + (q.size ? '?' + q.toString() : ''));
  }, [status, endpoint]);

  async function rememberTypes(rows: Delivery[]) {
    const missing = [...new Set(rows.map((d) => d.event_id))].filter((id) => !types.current.has(id));
    if (missing.length === 0) return;
    // One page of recent events covers most rows; the rest are fetched one by one.
    const recent = await client.listEvents(100);
    for (const e of recent.data) types.current.set(e.id, e.type);
    await Promise.all(missing.filter((id) => !types.current.has(id)).slice(0, 20).map(async (id) => types.current.set(id, (await client.getEvent(id)).type)));
  }

  async function loadMore() {
    const cursor = more.cursor ?? first.data?.next_cursor;
    if (!cursor) return;
    try {
      const page = await client.listDeliveries({ status: status === 'all' ? undefined : status, endpointId: endpoint || undefined, cursor, limit: 50 });
      await rememberTypes(page.data);
      setMore((m) => ({ rows: [...m.rows, ...page.data], cursor: page.next_cursor }));
    } catch (e) {
      setNotice({ ok: false, text: describeError(e) });
    }
  }

  const rows = [...(first.data?.data ?? []), ...more.rows];
  const nextCursor = more.rows.length ? more.cursor : first.data?.next_cursor ?? null;
  const hostOf = new Map((endpoints.data ?? []).map((e) => [e.id, target(e.url)]));
  const current = selected ?? rows[0]?.id ?? null;

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>Deliveries</h1>
          <p>One per event and endpoint, with every attempt kept.</p>
        </div>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center' }}>
          <label htmlFor="ep-filter" className="muted" style={{ fontSize: 13 }}>Endpoint</label>
          <select id="ep-filter" className="input" value={endpoint} onChange={(e) => { setEndpoint(e.target.value); setSelected(null); }}>
            <option value="">All endpoints</option>
            {(endpoints.data ?? []).map((e: Endpoint) => <option key={e.id} value={e.id}>{target(e.url)}</option>)}
          </select>
        </div>
      </div>

      <div role="group" aria-label="Filter by status" style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
        {filters.map((f) => (
          <button key={f.id} className="chip" type="button" aria-pressed={status === f.id} onClick={() => { setStatus(f.id); setSelected(null); }}>
            {f.label}
          </button>
        ))}
      </div>

      {notice && <div className={`notice ${notice.ok ? 'ok' : 'bad'}`} role="status">{notice.text}</div>}

      <div className="split">
        <section className="main card" aria-label="Deliveries" style={{ overflow: 'hidden' }}>
          {first.error !== null && !first.data && <ErrorNotice error={first.error} onRetry={first.reload} />}
          <div style={{ overflowX: 'auto' }}>
            <div style={{ minWidth: 680 }}>
              <div className="dhead" aria-hidden="true">
                <span>Status</span><span>Event</span><span>Endpoint</span><span>Created</span><span style={{ textAlign: 'right' }}>Attempts</span>
              </div>
              {!first.data && !first.error && [0, 1, 2, 3].map((i) => <div key={i} className="row"><Skeleton /></div>)}
              {rows.map((d) => (
                <button key={d.id} className="drow" type="button" aria-pressed={d.id === current} onClick={() => setSelected(d.id)}>
                  <span><StatusTag status={d.status} /></span>
                  <span className="mono ellipsis" style={{ fontSize: 13 }}>{types.current.get(d.event_id) ?? shortId(d.event_id)}</span>
                  <span className="mono ellipsis" style={{ fontSize: 13, color: 'var(--text-2)' }}>{hostOf.get(d.endpoint_id) ?? shortId(d.endpoint_id)}</span>
                  <span style={{ fontSize: 13, color: 'var(--muted)' }}>{relativeTime(d.created_at)}</span>
                  <span className="mono" style={{ fontSize: 13, textAlign: 'right' }}>{d.attempt_count}</span>
                </button>
              ))}
              {first.data && rows.length === 0 && <Empty>No deliveries match. Send an event to an active endpoint and it shows up here.</Empty>}
            </div>
          </div>
          {nextCursor && (
            <div style={{ padding: 14, borderTop: '1px solid var(--line)' }}>
              <button className="btn btn-ghost" type="button" onClick={loadMore}>Load older deliveries</button>
            </div>
          )}
        </section>

        <aside className="side card" aria-label="Delivery detail" style={{ padding: 22 }}>
          {current ? (
            <Detail
              key={current}
              id={current}
              eventType={(id) => types.current.get(id)}
              host={(id) => hostOf.get(id)}
              onReplayed={(text, ok) => { setNotice({ ok, text }); first.reload(); }}
            />
          ) : (
            <Empty>Select a delivery to see its attempts.</Empty>
          )}
        </aside>
      </div>
    </div>
  );
}

function Detail({ id, eventType, host, onReplayed }: { id: string; eventType: (id: string) => string | undefined; host: (id: string) => string | undefined; onReplayed: (text: string, ok: boolean) => void }) {
  const { client } = useSession();
  const [busy, setBusy] = useState(false);
  const d = usePolling(() => client.getDelivery(id), 3000, [id]);
  const [event, setEvent] = useState<Event | null>(null);

  useEffect(() => {
    if (d.data && !event) void client.getEvent(d.data.event_id).then(setEvent, () => undefined);
  }, [client, d.data, event]);

  if (d.error !== null && !d.data) return <ErrorNotice error={d.error} onRetry={d.reload} />;
  if (!d.data) return <Skeleton height={200} />;
  const del = d.data;
  const finished = del.status === 'dead' || del.status === 'succeeded';

  async function replay() {
    setBusy(true);
    try {
      await client.replay(id);
      onReplayed(`Replay queued for ${shortId(id)} · 202 Accepted`, true);
      d.reload();
    } catch (e) {
      onReplayed(e instanceof ApiError && e.status === 409 ? e.message : describeError(e), false);
    } finally {
      setBusy(false);
    }
  }

  const attempts = del.attempts ?? [];
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 18 }}>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center' }}>
        <StatusTag status={del.status} />
        <span className="mono muted" style={{ fontSize: 13 }}>{shortId(del.id)}</span>
      </div>
      <div>
        <div className="mono" style={{ fontSize: 18 }}>{event?.type ?? eventType(del.event_id) ?? shortId(del.event_id)}</div>
        <div className="mono muted" style={{ fontSize: 13, marginTop: 6 }}>→ {host(del.endpoint_id) ?? shortId(del.endpoint_id)}</div>
      </div>

      <ol className="timeline" aria-label="Attempts">
        {attempts.map((a) => {
          const kind = attemptKind(a);
          const dot = { ok: 'd-ok', fail: 'd-fail', interrupted: 'd-int', lost: 'd-int' }[kind];
          const outcome = a.status_code !== undefined ? `HTTP ${a.status_code}` : a.error ?? 'failed';
          return (
            <li key={a.attempt_no} className="step">
              <span className={`dot ${dot}`}>{a.attempt_no}</span>
              <div style={{ minWidth: 0, paddingTop: 6 }}>
                <div style={{ fontSize: 14, fontWeight: 500, wordBreak: 'break-word' }}>{kind === 'interrupted' ? 'Interrupted: the worker was stopping' : outcome}</div>
                <div className="mono muted" style={{ fontSize: 12, marginTop: 4 }}>{relativeTime(a.started_at)} · {duration(a.duration_ms)}</div>
                {kind === 'interrupted' && <div className="hint" style={{ marginTop: 6 }}>Recorded, but not counted against the endpoint or the retry budget.</div>}
                {kind === 'lost' && <div className="hint" style={{ marginTop: 6 }}>The worker stopped before reporting; this attempt counts.</div>}
                {a.status_code !== undefined && a.error && <div className="hint" style={{ marginTop: 6 }}>{a.error}</div>}
                {a.response_snippet && <div className="code" style={{ marginTop: 8, whiteSpace: 'pre-wrap', maxHeight: 120 }}>{a.response_snippet}</div>}
              </div>
            </li>
          );
        })}
        {del.status === 'pending' && (
          <li className="step">
            <span className="dot d-wait">{attempts.length + 1}</span>
            <div style={{ paddingTop: 6 }}>
              <div style={{ fontSize: 14, fontWeight: 500 }}>{del.next_attempt_at && Date.parse(del.next_attempt_at) > Date.now() ? `Next attempt ${relativeTime(del.next_attempt_at)}` : 'Due now'}</div>
              <div className="mono muted" style={{ fontSize: 12, marginTop: 4 }}>scheduled by the database clock</div>
            </div>
          </li>
        )}
        {del.status === 'in_flight' && (
          <li className="step">
            <span className="dot d-wait">{attempts.length + 1}</span>
            <div style={{ paddingTop: 6 }}>
              <div style={{ fontSize: 14, fontWeight: 500 }}>Sending…</div>
              <div className="mono muted" style={{ fontSize: 12, marginTop: 4 }}>leased to a worker</div>
            </div>
          </li>
        )}
        {attempts.length === 0 && del.status !== 'pending' && del.status !== 'in_flight' && <li className="hint">No attempts were made (its endpoint was disabled).</li>}
      </ol>

      {event && (
        <details>
          <summary style={{ cursor: 'pointer', minHeight: 32, display: 'flex', alignItems: 'center', fontSize: 13, color: 'var(--muted)' }}>Payload</summary>
          <div className="code" style={{ marginTop: 8, whiteSpace: 'pre-wrap', maxHeight: 240 }}>{JSON.stringify(event.payload, null, 2)}</div>
        </details>
      )}

      <div style={{ borderTop: '1px solid var(--line)', paddingTop: 16, display: 'flex', flexDirection: 'column', gap: 10 }}>
        <button className="btn btn-primary" type="button" onClick={replay} disabled={!finished || busy}>
          {busy ? 'Queuing…' : 'Replay with a fresh retry budget'}
        </button>
        <div className="hint">
          {finished
            ? 'The history is kept; new attempts are numbered after it. Two replays at once queue it once.'
            : 'Only a succeeded or dead delivery can be replayed: this one is still being delivered.'}
        </div>
      </div>
    </div>
  );
}
