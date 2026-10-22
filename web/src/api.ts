// The console's client for the Hook Mini API. Same origin, Bearer key, JSON.
// Every error becomes an ApiError carrying the documented envelope (code,
// message, field, request_id) and, for a 429, how long to wait.

import type { Delivery, DeliveryStatus, Endpoint, Event, Page, Stats } from './types';

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly field: string | undefined;
  readonly requestId: string | undefined;
  /** Seconds, from Retry-After on a 429. */
  readonly retryAfter: number | undefined;

  constructor(status: number, code: string, message: string, extra: { field?: string | undefined; requestId?: string | undefined; retryAfter?: number | undefined } = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.field = extra.field;
    this.requestId = extra.requestId;
    this.retryAfter = extra.retryAfter;
  }
}

export interface RateLimit {
  limit: number;
  remaining: number;
}

export interface ClientOptions {
  getKey: () => string | null;
  baseUrl?: string;
  fetch?: typeof fetch;
  /** Called with the budget from every response that carries one. */
  onRateLimit?: (r: RateLimit) => void;
  /** Called on a 401: the key is no longer accepted. */
  onUnauthorized?: () => void;
}

/** Reads RateLimit-Limit/-Remaining; null when absent or malformed. */
export function readRateLimit(h: Headers): RateLimit | null {
  const limit = Number.parseInt(h.get('RateLimit-Limit') ?? '', 10);
  const remaining = Number.parseInt(h.get('RateLimit-Remaining') ?? '', 10);
  if (!Number.isFinite(limit) || !Number.isFinite(remaining) || limit <= 0 || remaining < 0) return null;
  return { limit, remaining: Math.min(remaining, limit) };
}

/** Builds an ApiError from a failed response, whatever its body. */
export async function toApiError(res: Response): Promise<ApiError> {
  const requestId = res.headers.get('X-Request-Id') ?? undefined;
  const ra = Number.parseInt(res.headers.get('Retry-After') ?? '', 10);
  const retryAfter = res.status === 429 && Number.isFinite(ra) && ra > 0 ? ra : undefined;
  let body: unknown = null;
  try {
    body = await res.json();
  } catch {
    // not JSON (a proxy's error page, an empty body): fall through
  }
  const e = (body as { error?: { code?: unknown; message?: unknown; field?: unknown; request_id?: unknown } } | null)?.error;
  if (e && typeof e.code === 'string' && typeof e.message === 'string') {
    return new ApiError(res.status, e.code, e.message, {
      field: typeof e.field === 'string' ? e.field : undefined,
      requestId: typeof e.request_id === 'string' ? e.request_id : requestId,
      retryAfter,
    });
  }
  return new ApiError(res.status, 'http_' + res.status, `The server answered ${res.status} ${res.statusText}`.trim(), { requestId, retryAfter });
}

export interface Response2<T> {
  data: T;
  status: number;
  headers: Headers;
}

export function createClient(opts: ClientOptions) {
  const base = opts.baseUrl ?? '';
  const doFetch = opts.fetch ?? ((...a: Parameters<typeof fetch>) => fetch(...a));

  async function request<T>(method: string, path: string, init: { body?: unknown; headers?: Record<string, string>; query?: Record<string, string | undefined> } = {}): Promise<Response2<T>> {
    const key = opts.getKey();
    if (!key) throw new ApiError(401, 'unauthorized', 'Not signed in');
    const qs = new URLSearchParams();
    for (const [k, v] of Object.entries(init.query ?? {})) if (v !== undefined && v !== '') qs.set(k, v);
    const url = base + path + (qs.size ? '?' + qs.toString() : '');
    const headers: Record<string, string> = { Authorization: 'Bearer ' + key, Accept: 'application/json', ...init.headers };
    let body: string | undefined;
    if (init.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(init.body);
    }
    let res: Response;
    try {
      res = await doFetch(url, { method, headers, ...(body !== undefined ? { body } : {}), credentials: 'omit', cache: 'no-store' });
    } catch {
      throw new ApiError(0, 'network', 'The API could not be reached');
    }
    const rl = readRateLimit(res.headers);
    if (rl) opts.onRateLimit?.(rl);
    if (!res.ok) {
      const err = await toApiError(res);
      if (res.status === 401) opts.onUnauthorized?.();
      throw err;
    }
    const data = (res.status === 204 ? undefined : await res.json()) as T;
    return { data, status: res.status, headers: res.headers };
  }

  const get = async <T>(path: string, query?: Record<string, string | undefined>) => (await request<T>('GET', path, query ? { query } : {})).data;
  const post = async <T>(path: string, body?: unknown) => (await request<T>('POST', path, body === undefined ? {} : { body })).data;

  return {
    request,
    stats: () => get<Stats>('/v1/stats'),
    listEndpoints: (cursor?: string) => get<Page<Endpoint>>('/v1/endpoints', { limit: '100', cursor }),
    createEndpoint: (url: string, description: string) => post<Endpoint>('/v1/endpoints', { url, description }),
    rotateSecret: (id: string) => post<Endpoint>(`/v1/endpoints/${encodeURIComponent(id)}/rotate-secret`),
    disableEndpoint: (id: string) => post<Endpoint>(`/v1/endpoints/${encodeURIComponent(id)}/disable`),
    enableEndpoint: (id: string) => post<Endpoint>(`/v1/endpoints/${encodeURIComponent(id)}/enable`),
    deleteEndpoint: async (id: string) => {
      await request<void>('DELETE', `/v1/endpoints/${encodeURIComponent(id)}`);
    },
    listDeliveries: (f: { status?: DeliveryStatus | undefined; endpointId?: string | undefined; eventId?: string | undefined; cursor?: string | undefined; limit?: number }) =>
      get<Page<Delivery>>('/v1/deliveries', { status: f.status, endpoint_id: f.endpointId, event_id: f.eventId, cursor: f.cursor, limit: String(f.limit ?? 50) }),
    getDelivery: (id: string) => get<Delivery>(`/v1/deliveries/${encodeURIComponent(id)}`),
    replay: (id: string) => post<Delivery>(`/v1/deliveries/${encodeURIComponent(id)}/replay`),
    listEvents: (limit = 100) => get<Page<Event>>('/v1/events', { limit: String(limit) }),
    getEvent: (id: string) => get<Event>(`/v1/events/${encodeURIComponent(id)}`),
    /** replayed is true when the Idempotency-Key matched an earlier event. */
    createEvent: async (type: string, payload: unknown, idempotencyKey?: string) => {
      const r = await request<Event>('POST', '/v1/events', { body: { type, payload }, ...(idempotencyKey ? { headers: { 'Idempotency-Key': idempotencyKey } } : {}) });
      return { event: r.data, status: r.status, replayed: r.headers.get('Idempotent-Replayed') === 'true' };
    },
  };
}

export type Client = ReturnType<typeof createClient>;

/** A message fit for the user: what went wrong and, for a 429, when to retry. */
export function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 429) return `Too many requests for this tenant. Try again in ${e.retryAfter ?? 1} s.`;
    if (e.status === 0) return 'The API could not be reached. Is it running?';
    if (e.status >= 500) return `Something went wrong on the server.${e.requestId ? ` Request ID ${e.requestId}.` : ''}`;
    return e.message;
  }
  return 'Something unexpected happened.';
}
