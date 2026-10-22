// Shapes of the API's responses, as api/openapi.yaml documents them.

export type DeliveryStatus = 'pending' | 'in_flight' | 'succeeded' | 'dead';

export interface Page<T> {
  data: T[];
  next_cursor: string | null;
}

export interface Endpoint {
  id: string;
  url: string;
  description: string;
  status: 'active' | 'disabled';
  consecutive_failures: number;
  disabled_at?: string;
  disabled_reason?: string;
  previous_secret_expires_at?: string;
  /** Only in the responses that create the endpoint or rotate its secret. */
  secret?: string;
  created_at: string;
  updated_at: string;
}

export interface Event {
  id: string;
  type: string;
  payload: Record<string, unknown>;
  idempotency_key?: string;
  created_at: string;
  delivery_count?: number;
}

export interface Attempt {
  attempt_no: number;
  started_at: string;
  duration_ms: number;
  status_code?: number;
  response_snippet?: string;
  error?: string;
}

export interface Delivery {
  id: string;
  event_id: string;
  endpoint_id: string;
  status: DeliveryStatus;
  attempt_count: number;
  next_attempt_at?: string;
  last_status_code?: number;
  last_error?: string;
  completed_at?: string;
  created_at: string;
  attempts?: Attempt[];
}

export interface StatusCounts {
  succeeded: number;
  dead: number;
  pending: number;
  in_flight: number;
}

export interface Stats {
  last_24h: StatusCounts;
  backlog: { pending: number; in_flight: number };
  endpoints: {
    id: string;
    url: string;
    status: 'active' | 'disabled';
    consecutive_failures: number;
    last_24h: StatusCounts;
  }[];
}
