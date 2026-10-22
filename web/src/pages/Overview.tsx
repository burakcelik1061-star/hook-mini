import { useRef, useState } from 'react';
import { describeError } from '../api';
import { Pipeline } from '../components/Pipeline';
import { Empty, ErrorNotice, Skeleton, usePolling } from '../components/ui';
import { count, target, percent, relativeTime, shortId, successRate } from '../format';
import { Link, paths } from '../router';
import { useSession } from '../session';

export function Overview() {
  const { client } = useSession();
  const stats = usePolling(() => client.stats(), 5000);
  const recent = usePolling(async () => (await client.listDeliveries({ limit: 50 })).data, 2000);
  const dead = usePolling(async () => {
    const page = await client.listDeliveries({ status: 'dead', limit: 5 });
    return Promise.all(page.data.map(async (d) => ({ ...d, type: await eventType(d.event_id) })));
  }, 10000);
  const types = useRef(new Map<string, string>());
  const [notice, setNotice] = useState<{ ok: boolean; text: string } | null>(null);

  async function eventType(id: string): Promise<string> {
    const known = types.current.get(id);
    if (known) return known;
    const t = (await client.getEvent(id)).type;
    types.current.set(id, t);
    return t;
  }

  async function replay(id: string) {
    try {
      await client.replay(id);
      setNotice({ ok: true, text: `Replay queued for ${shortId(id)} · 202 Accepted` });
      dead.reload();
      stats.reload();
    } catch (e) {
      setNotice({ ok: false, text: describeError(e) });
    }
  }

  async function enable(id: string) {
    try {
      await client.enableEndpoint(id);
      setNotice({ ok: true, text: 'Endpoint enabled. Replay its dead deliveries to send them again.' });
      stats.reload();
    } catch (e) {
      setNotice({ ok: false, text: describeError(e) });
    }
  }

  const s = stats.data;
  const hostOf = new Map(s?.endpoints.map((e) => [e.id, target(e.url)]) ?? []);
  const disabled = s?.endpoints.filter((e) => e.status === 'disabled') ?? [];
  const backlog = s ? s.backlog.pending + s.backlog.in_flight : 0;

  return (
    <div className="page">
      {stats.error !== null && !s && <ErrorNotice error={stats.error} onRetry={stats.reload} />}
      {notice && (
        <div className={`notice ${notice.ok ? 'ok' : 'bad'}`} role="status">
          {notice.text}
        </div>
      )}

      <section style={{ display: 'flex', flexWrap: 'wrap', gap: 28, alignItems: 'stretch' }}>
        <div style={{ flex: '1 1 420px', minWidth: 0, display: 'flex', flexDirection: 'column', gap: 22, paddingTop: 12 }}>
          <div>
            {s ? (
              <span className={`tag ${disabled.length ? 't-warn' : 't-ok'}`}>{disabled.length ? `◆ ${disabled.length} endpoint${disabled.length > 1 ? 's' : ''} disabled` : '● All endpoints active'}</span>
            ) : (
              <Skeleton width={160} height={24} />
            )}
          </div>
          <h1 style={{ margin: 0, fontSize: 52, lineHeight: 1.04, letterSpacing: '-0.035em', fontWeight: 700 }}>
            Every webhook,
            <br />
            accounted for.
          </h1>
          <p style={{ margin: 0, fontSize: 17, lineHeight: 1.6, color: 'var(--muted)', maxWidth: '46ch' }}>Signed, retried with backoff, and fenced so no attempt is recorded twice. Watch your deliveries move from the queue to your endpoints.</p>
          <div className="kpis">
            <Kpi label="Succeeded · 24 h" value={s && count(s.last_24h.succeeded)} />
            <Kpi label="Dead · 24 h" value={s && count(s.last_24h.dead)} color={s?.last_24h.dead ? 'var(--bad)' : undefined} />
            <Kpi label="Waiting to send or retry" value={s && count(s.backlog.pending)} color={s?.backlog.pending ? 'var(--warn)' : undefined} />
            <Kpi label="In flight now" value={s && count(s.backlog.in_flight)} />
          </div>
        </div>

        <figure className="card" style={{ flex: '1.35 1 560px', minWidth: 0, margin: 0, padding: '20px 22px 8px', overflow: 'hidden' }}>
          <figcaption style={{ display: 'flex', flexWrap: 'wrap', justifyContent: 'space-between', gap: 12, alignItems: 'center' }}>
            <span style={{ fontSize: 14, fontWeight: 600 }}>Live pipeline</span>
            <span style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
              <span className="tag t-ok">● delivered</span>
              <span className="tag t-warn">◆ failed, will retry</span>
              <span className="tag t-bad">✕ dead</span>
            </span>
          </figcaption>
          {s && s.endpoints.length === 0 ? (
            <Empty>
              No endpoints yet. <Link to={paths.endpoints}>Add one</Link>, then <Link to={paths.send}>send an event</Link> and watch it arrive here.
            </Empty>
          ) : (
            <Pipeline endpoints={s?.endpoints ?? []} recent={recent.data} />
          )}
          <div className="hint" style={{ padding: '0 0 10px' }}>
            {backlog ? `${count(backlog)} ${backlog === 1 ? 'delivery' : 'deliveries'} not finished yet.` : 'The queue is empty.'} Updates every 2 s while this tab is open.
          </div>
        </figure>
      </section>

      <section style={{ display: 'flex', flexWrap: 'wrap', gap: 28 }}>
        <div className="card" style={{ flex: '1 1 520px', minWidth: 0, overflow: 'hidden' }}>
          <div className="card-head">
            <h2>Needs attention</h2>
            <Link to={paths.deliveries} query="?status=dead" style={{ fontSize: 14, minHeight: 44, display: 'inline-flex', alignItems: 'center' }}>
              All dead deliveries →
            </Link>
          </div>
          {disabled.map((e) => (
            <div className="row" key={e.id} style={{ background: 'rgba(255,122,110,0.05)' }}>
              <span className="tag t-bad">✕ disabled</span>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div className="mono ellipsis" style={{ fontSize: 13 }}>
                  {target(e.url)}
                </div>
                <div className="hint">{e.consecutive_failures > 0 ? `${e.consecutive_failures} failed attempts in a row.` : 'Disabled.'} New events are not delivered to it.</div>
              </div>
              <button className="btn btn-ghost" type="button" onClick={() => enable(e.id)}>
                Enable
              </button>
            </div>
          ))}
          {dead.data?.map((d) => (
            <div className="row" key={d.id}>
              <span className="tag t-bad">✕ dead</span>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div className="mono ellipsis" style={{ fontSize: 13 }}>
                  {d.type} → {hostOf.get(d.endpoint_id) ?? shortId(d.endpoint_id)}
                </div>
                <div className="hint ellipsis">
                  {d.last_error ?? 'failed'} · {relativeTime(d.completed_at)}
                </div>
              </div>
              <button className="btn btn-ghost" type="button" onClick={() => replay(d.id)} aria-label={`Replay delivery ${shortId(d.id)}`}>
                Replay
              </button>
            </div>
          ))}
          {dead.data && dead.data.length === 0 && disabled.length === 0 && <Empty>Nothing needs attention: no dead deliveries, no disabled endpoints.</Empty>}
          {!dead.data && !dead.error && (
            <div className="row">
              <Skeleton />
            </div>
          )}
          {dead.error !== null && !dead.data && <ErrorNotice error={dead.error} onRetry={dead.reload} />}
        </div>

        <div className="card" style={{ flex: '1 1 520px', minWidth: 0, overflow: 'hidden' }}>
          <div className="card-head">
            <h2>Endpoints · 24 h</h2>
            <Link to={paths.endpoints} style={{ fontSize: 14, minHeight: 44, display: 'inline-flex', alignItems: 'center' }}>
              Manage →
            </Link>
          </div>
          <div style={{ overflowX: 'auto' }}>
            {s?.endpoints.map((e) => {
              const rate = successRate(e.last_24h.succeeded, e.last_24h.dead);
              const color = e.status === 'disabled' ? 'var(--bad)' : rate !== null && rate < 95 ? 'var(--warn)' : 'var(--accent)';
              return (
                <div className="row" key={e.id} style={{ minWidth: 520, flexWrap: 'nowrap' }}>
                  <div className="mono ellipsis" style={{ flex: '0 0 210px', fontSize: 13 }}>
                    {target(e.url)}
                  </div>
                  <div className="bar" aria-hidden="true">
                    <span style={{ width: `${rate ?? 0}%`, background: color }} />
                  </div>
                  <div className="mono" style={{ flex: '0 0 64px', textAlign: 'right', fontSize: 13 }} title="Succeeded among finished deliveries">
                    {percent(rate)}
                  </div>
                  <div className="mono muted" style={{ flex: '0 0 90px', textAlign: 'right', fontSize: 13 }} title="In flight now">
                    {e.last_24h.in_flight} in flight
                  </div>
                </div>
              );
            })}
            {s && s.endpoints.length === 0 && <Empty>No endpoints yet.</Empty>}
          </div>
        </div>
      </section>
    </div>
  );
}

function Kpi({ label, value, color }: { label: string; value: string | null | undefined; color?: string | undefined }) {
  return (
    <div className="card kpi">
      <div className="kpi-label">{label}</div>
      <div className="kpi-value" style={color ? { color } : undefined}>
        {value ?? <Skeleton height={34} width={120} />}
      </div>
    </div>
  );
}
