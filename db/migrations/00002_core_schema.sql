-- +goose Up
-- Core domain schema. Design rules, in order of importance:
--   1. The database enforces invariants, not just the application. A bug in a
--      query must not be able to leak one tenant's data into another's.
--   2. Statuses are text + CHECK rather than enum types: adding a state later
--      is a one-line constraint change instead of an ALTER TYPE dance.
--   3. Every table a worker or listing endpoint scans has an index shaped for
--      exactly that query.

CREATE TABLE tenants (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 200),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER tenants_set_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- API keys. The secret itself is never stored: key_hash is the SHA-256 of the
-- full key. SHA-256 (not a slow password hash) is deliberate: keys are 256-bit
-- random values, so there is nothing to brute-force. prefix is the short,
-- non-secret part shown in dashboards and used to look the row up.
CREATE TABLE api_keys (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name         text        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 100),
    prefix       text        NOT NULL UNIQUE CHECK (char_length(prefix) BETWEEN 8 AND 32),
    key_hash     bytea       NOT NULL CHECK (octet_length(key_hash) = 32),
    last_used_at timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX api_keys_tenant_idx ON api_keys (tenant_id, created_at DESC, id DESC);

-- Subscriber endpoints. secret is stored readable on purpose: signing a
-- request needs the raw value, so it cannot be hashed. (Encryption at rest is
-- a documented limit of the MVP.)
CREATE TABLE endpoints (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    url                  text        NOT NULL CHECK (url ~* '^https?://[^[:space:]]+$' AND char_length(url) <= 2048),
    description          text        NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    secret               text        NOT NULL CHECK (char_length(secret) >= 24),
    status               text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    consecutive_failures integer     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    disabled_at          timestamptz,
    disabled_reason      text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    -- A disabled endpoint must say when it was disabled; an active one must not.
    CONSTRAINT endpoints_disabled_consistent CHECK ((status = 'disabled') = (disabled_at IS NOT NULL)),
    -- Target of the composite foreign key from deliveries (see below).
    CONSTRAINT endpoints_id_tenant_key UNIQUE (id, tenant_id)
);

CREATE INDEX endpoints_tenant_idx ON endpoints (tenant_id, created_at DESC, id DESC);
-- Fan-out reads "all active endpoints of a tenant" on every ingested event.
CREATE INDEX endpoints_active_idx ON endpoints (tenant_id) WHERE status = 'active';

CREATE TRIGGER endpoints_set_updated_at
    BEFORE UPDATE ON endpoints
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE events (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    type            text        NOT NULL CHECK (char_length(type) <= 128 AND type ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    payload         jsonb       NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    idempotency_key text        CHECK (char_length(idempotency_key) BETWEEN 1 AND 255),
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Replaying a request with the same key must not create a second event.
    -- NULL keys are distinct, so clients that send no key are unaffected.
    CONSTRAINT events_idempotency_key UNIQUE (tenant_id, idempotency_key),
    CONSTRAINT events_id_tenant_key UNIQUE (id, tenant_id)
);

CREATE INDEX events_tenant_idx ON events (tenant_id, created_at DESC, id DESC);

-- One row per (event, endpoint): the unit of work the worker retries.
--
-- Lifecycle: pending -> in_flight -> succeeded
--                  ^        |
--                  +--------+  (retry: back to pending with a later next_attempt_at)
--            in_flight -> dead   (attempts exhausted)
--
-- A retry waiting for its next attempt is "pending" with a future
-- next_attempt_at, so the worker needs one predicate to find work.
CREATE TABLE deliveries (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL,
    event_id         uuid        NOT NULL,
    endpoint_id      uuid        NOT NULL,
    status           text        NOT NULL DEFAULT 'pending'
                                 CHECK (status IN ('pending', 'in_flight', 'succeeded', 'dead')),
    attempt_count    integer     NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    -- A worker leases a delivery for a bounded time. If it crashes, the lease
    -- expires and another worker may reclaim the row.
    locked_until     timestamptz,
    locked_by        text,
    last_status_code integer     CHECK (last_status_code BETWEEN 100 AND 599),
    last_error       text,
    completed_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    -- Composite foreign keys: a delivery can only reference an event and an
    -- endpoint that belong to the SAME tenant as the delivery itself.
    CONSTRAINT deliveries_event_fk    FOREIGN KEY (event_id, tenant_id)
        REFERENCES events (id, tenant_id) ON DELETE CASCADE,
    CONSTRAINT deliveries_endpoint_fk FOREIGN KEY (endpoint_id, tenant_id)
        REFERENCES endpoints (id, tenant_id) ON DELETE CASCADE,

    -- Fan-out must be idempotent: the same event never targets an endpoint twice.
    CONSTRAINT deliveries_event_endpoint_key UNIQUE (event_id, endpoint_id),

    CONSTRAINT deliveries_lease_consistent CHECK ((status = 'in_flight') = (locked_until IS NOT NULL)),
    CONSTRAINT deliveries_completed_consistent CHECK ((status IN ('succeeded', 'dead')) = (completed_at IS NOT NULL))
);

-- The worker's hot query: "next due deliveries". Partial, so it stays small
-- no matter how many finished rows accumulate.
CREATE INDEX deliveries_due_idx ON deliveries (next_attempt_at) WHERE status = 'pending';
-- Reclaiming deliveries whose worker died while holding a lease.
CREATE INDEX deliveries_lease_idx ON deliveries (locked_until) WHERE status = 'in_flight';
-- History views: by endpoint, and by tenant newest-first.
CREATE INDEX deliveries_endpoint_idx ON deliveries (endpoint_id, created_at DESC, id DESC);
CREATE INDEX deliveries_tenant_idx ON deliveries (tenant_id, created_at DESC, id DESC);

CREATE TRIGGER deliveries_set_updated_at
    BEFORE UPDATE ON deliveries
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Append-only log of every HTTP attempt, kept apart from deliveries so the
-- hot table stays narrow and the history is never overwritten.
CREATE TABLE delivery_attempts (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    delivery_id      uuid        NOT NULL REFERENCES deliveries (id) ON DELETE CASCADE,
    attempt_no       integer     NOT NULL CHECK (attempt_no >= 1),
    started_at       timestamptz NOT NULL,
    duration_ms      integer     NOT NULL CHECK (duration_ms >= 0),
    status_code      integer     CHECK (status_code BETWEEN 100 AND 599),
    response_snippet text        CHECK (char_length(response_snippet) <= 1024),
    error            text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT delivery_attempts_delivery_no_key UNIQUE (delivery_id, attempt_no)
);

-- +goose Down
DROP TABLE delivery_attempts;
DROP TABLE deliveries;
DROP TABLE events;
DROP TABLE endpoints;
DROP TABLE api_keys;
DROP TABLE tenants;
