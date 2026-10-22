import { describe, expect, it, vi } from 'vitest';
import { ApiError, createClient, describeError, readRateLimit, toApiError } from './api';

const KEY = 'hm_0123abcd_' + 'a'.repeat(64);

function json(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
}

function client(respond: (url: string, init: RequestInit) => Response | Promise<Response>, extra: { onRateLimit?: (r: unknown) => void; onUnauthorized?: () => void } = {}) {
  const calls: { url: string; init: RequestInit }[] = [];
  const c = createClient({
    getKey: () => KEY,
    baseUrl: 'http://api.test',
    fetch: (async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return respond(url, init);
    }) as unknown as typeof fetch,
    ...extra,
  });
  return { c, calls };
}

describe('the request', () => {
  it('sends the key as a Bearer token, JSON bodies, and no cookies', async () => {
    const { c, calls } = client(() => json(201, { id: 'e1' }));
    await c.createEndpoint('https://x.example', 'd');
    const { url, init } = calls[0]!;
    expect(url).toBe('http://api.test/v1/endpoints');
    expect(init.method).toBe('POST');
    const h = init.headers as Record<string, string>;
    expect(h.Authorization).toBe('Bearer ' + KEY);
    expect(h['Content-Type']).toBe('application/json');
    expect(JSON.parse(init.body as string)).toEqual({ url: 'https://x.example', description: 'd' });
    expect(init.credentials).toBe('omit');
  });

  it('puts filters in the query and leaves out empty ones', async () => {
    const { c, calls } = client(() => json(200, { data: [], next_cursor: null }));
    await c.listDeliveries({ status: 'dead', endpointId: undefined, eventId: '', limit: 5 });
    expect(calls[0]!.url).toBe('http://api.test/v1/deliveries?status=dead&limit=5');
  });

  it('escapes ids in paths', async () => {
    const { c, calls } = client(() => json(202, {}));
    await c.replay('a/../b');
    expect(calls[0]!.url).toBe('http://api.test/v1/deliveries/a%2F..%2Fb/replay');
  });

  it('sends the Idempotency-Key and reports a replayed event', async () => {
    const { c, calls } = client(() => json(200, { id: 'ev', type: 'a.b', payload: {}, created_at: '' }, { 'Idempotent-Replayed': 'true' }));
    const r = await c.createEvent('a.b', {}, 'order-1');
    expect((calls[0]!.init.headers as Record<string, string>)['Idempotency-Key']).toBe('order-1');
    expect(r.replayed).toBe(true);
    expect(r.status).toBe(200);
  });

  it('does not send an Idempotency-Key when there is none', async () => {
    const { c, calls } = client(() => json(201, { id: 'ev', type: 'a.b', payload: {}, created_at: '' }));
    const r = await c.createEvent('a.b', {});
    expect((calls[0]!.init.headers as Record<string, string>)['Idempotency-Key']).toBeUndefined();
    expect(r.replayed).toBe(false);
  });

  it('refuses to send without a key', async () => {
    const fetch = vi.fn();
    const c = createClient({ getKey: () => null, fetch: fetch as unknown as typeof globalThis.fetch });
    await expect(c.stats()).rejects.toMatchObject({ status: 401 });
    expect(fetch).not.toHaveBeenCalled();
  });

  it('handles 204 with no body', async () => {
    const { c } = client(() => new Response(null, { status: 204 }));
    await expect(c.deleteEndpoint('x')).resolves.toBeUndefined();
  });
});

describe('errors', () => {
  it('reads the documented envelope', async () => {
    const e = await toApiError(json(400, { error: { code: 'invalid_request', message: 'url must include a host', field: 'url', request_id: 'R1' } }));
    expect(e).toBeInstanceOf(ApiError);
    expect(e).toMatchObject({ status: 400, code: 'invalid_request', message: 'url must include a host', field: 'url', requestId: 'R1' });
  });

  it('copes with a body that is not the envelope', async () => {
    const e = await toApiError(new Response('<html>bad gateway</html>', { status: 502, statusText: 'Bad Gateway', headers: { 'X-Request-Id': 'R2' } }));
    expect(e).toMatchObject({ status: 502, code: 'http_502', requestId: 'R2' });
  });

  it('keeps Retry-After only for a 429', async () => {
    const limited = await toApiError(json(429, { error: { code: 'rate_limited', message: 'slow down' } }, { 'Retry-After': '3' }));
    expect(limited.retryAfter).toBe(3);
    const other = await toApiError(json(503, { error: { code: 'x', message: 'y' } }, { 'Retry-After': '3' }));
    expect(other.retryAfter).toBeUndefined();
  });

  it('tells the session about a revoked key, and only on a 401', async () => {
    const onUnauthorized = vi.fn();
    const { c } = client(() => json(401, { error: { code: 'unauthorized', message: 'no' } }), { onUnauthorized });
    await expect(c.stats()).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).toHaveBeenCalledTimes(1);

    const notAuth = vi.fn();
    const { c: c2 } = client(() => json(404, { error: { code: 'not_found', message: 'no' } }), { onUnauthorized: notAuth });
    await expect(c2.getDelivery('x')).rejects.toMatchObject({ status: 404 });
    expect(notAuth).not.toHaveBeenCalled();
  });

  it('turns a network failure into an ApiError with status 0', async () => {
    const { c } = client(() => {
      throw new TypeError('failed to fetch');
    });
    await expect(c.stats()).rejects.toMatchObject({ status: 0, code: 'network' });
  });

  it('describes errors for people, never leaking internals', () => {
    expect(describeError(new ApiError(429, 'rate_limited', 'x', { retryAfter: 4 }))).toContain('4 s');
    expect(describeError(new ApiError(500, 'internal_error', 'pq: relation does not exist', { requestId: 'R9' }))).toBe('Something went wrong on the server. Request ID R9.');
    expect(describeError(new ApiError(409, 'endpoint_disabled', 'the endpoint is disabled'))).toBe('the endpoint is disabled');
    expect(describeError(new Error('boom'))).toBe('Something unexpected happened.');
  });
});

describe('the request budget', () => {
  it('is read from every response that carries it, failures included', async () => {
    const onRateLimit = vi.fn();
    const { c } = client(() => json(429, { error: { code: 'rate_limited', message: 'x' } }, { 'RateLimit-Limit': '100', 'RateLimit-Remaining': '0', 'Retry-After': '1' }), { onRateLimit });
    await expect(c.stats()).rejects.toBeInstanceOf(ApiError);
    expect(onRateLimit).toHaveBeenCalledWith({ limit: 100, remaining: 0 });
  });

  it('ignores missing or nonsensical headers', () => {
    expect(readRateLimit(new Headers())).toBeNull();
    expect(readRateLimit(new Headers({ 'RateLimit-Limit': 'x', 'RateLimit-Remaining': '1' }))).toBeNull();
    expect(readRateLimit(new Headers({ 'RateLimit-Limit': '0', 'RateLimit-Remaining': '0' }))).toBeNull();
    expect(readRateLimit(new Headers({ 'RateLimit-Limit': '10', 'RateLimit-Remaining': '-1' }))).toBeNull();
    expect(readRateLimit(new Headers({ 'RateLimit-Limit': '10', 'RateLimit-Remaining': '50' }))).toEqual({ limit: 10, remaining: 10 });
  });
});
