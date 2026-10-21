# Hook Mini

A small webhook delivery service: applications send events, Hook Mini delivers
them to subscriber endpoints with signed requests, retries and a delivery
history. Think Svix, scaled down to something you can read in an afternoon.

> **Status: Phase 2 (API).** You can register endpoints and submit events, and
> every event is durably queued as one delivery per active endpoint. **Nothing
> is sent to the endpoints yet**: the delivery engine (workers, retries,
> signing) arrives in Phases 3 and 4. See [Roadmap](#roadmap).

## Architecture

```mermaid
flowchart LR
    client[Client app] -->|HTTP| api
    subgraph Hook Mini
        api[api :8080]
        worker[worker]
        pg[(PostgreSQL)]
        api --> pg
        worker --> pg
    end
    worker -.->|signed webhook, Phase 3-4| sub[Subscriber endpoint]
```

Two independent processes, one infrastructure dependency:

- **api** serves HTTP. **worker** has no port and only talks to Postgres.
- **PostgreSQL is the only service.** The job queue will use
  `FOR UPDATE SKIP LOCKED` instead of Redis, so there is nothing else to run,
  back up or keep consistent with the database.
- **migrate** is a third binary that applies the embedded SQL migrations and
  exits; compose starts the api and worker only after it succeeds.

## Data model

```mermaid
erDiagram
    tenants ||--o{ api_keys : has
    tenants ||--o{ endpoints : has
    tenants ||--o{ events : sends
    events ||--o{ deliveries : "fans out to"
    endpoints ||--o{ deliveries : receives
    deliveries ||--o{ delivery_attempts : records
```

- **Tenant isolation is enforced by the database.** `deliveries` references
  `events` and `endpoints` through *composite* foreign keys
  `(id, tenant_id)`, so a delivery can never link one tenant's event to
  another tenant's endpoint, even if a query is wrong.
- **Fan-out is one statement** (`INSERT ... SELECT` over the tenant's active
  endpoints) and idempotent: `UNIQUE (event_id, endpoint_id)`.
- **Event ingestion is idempotent**: `UNIQUE (tenant_id, idempotency_key)`.
- **A delivery's lifecycle is constrained in SQL**: `in_flight` requires a
  lease, finished states require `completed_at`. A worker bug that forgets to
  clear a lease is rejected by Postgres instead of corrupting the queue.
- **Retries and lease expiry share one hot path**: partial indexes
  (`WHERE status = 'pending'`, `WHERE status = 'in_flight'`) stay small no
  matter how many finished deliveries accumulate. A test asserts the planner
  actually picks the due-work index for the worker's "next due" query.
- **API keys are stored as SHA-256 hashes**, looked up by a public prefix.
  They are 256-bit random values, so a slow password hash would add cost
  without adding security.
- **Typed queries come from sqlc**, generated from the same SQL migrations
  that create the schema, so the two cannot drift; CI fails if they do.

## Quick start

Requires Docker.

```bash
docker compose up -d --build
```

Tenants are created by an operator, not over HTTP. This prints the first API
key; it is shown once and only its hash is stored:

```bash
docker compose run --rm api admin bootstrap "Acme Inc"
```

```bash
export KEY=hm_...   # the api_key from the output above
```

Register an endpoint (the response contains its signing secret, **once**):

```bash
curl -s -X POST localhost:8080/v1/endpoints \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/hooks/billing", "description": "billing"}'
```

Submit an event. Retrying with the same `Idempotency-Key` is safe:

```bash
curl -s -X POST localhost:8080/v1/events \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-1001" \
  -d '{"type": "invoice.paid", "payload": {"amount": 42, "currency": "EUR"}}'
```

```bash
curl localhost:8080/healthz
curl localhost:8080/readyz
```

## API

All `/v1` routes need `Authorization: Bearer <api key>`. Bodies are JSON.

| Method and path | Purpose |
| --- | --- |
| `POST /v1/endpoints` | Register an endpoint. `201`; the response is the only one that contains `secret`. |
| `GET /v1/endpoints` | List endpoints (newest first, paginated). |
| `GET /v1/endpoints/{id}` | One endpoint. |
| `PATCH /v1/endpoints/{id}` | Change `url` and/or `description`. |
| `DELETE /v1/endpoints/{id}` | Delete an endpoint and its delivery history. `204`. |
| `POST /v1/events` | Submit an event. `201` when new, `200` on an idempotent replay. |
| `GET /v1/events` | List events (newest first, paginated). |
| `GET /v1/events/{id}` | One event with its `delivery_count`. |

**Idempotency.** With an `Idempotency-Key` header, the same key and the same
content returns the original event (`200`, `Idempotent-Replayed: true`) and
creates nothing. The same key with different content is `422
idempotency_key_reuse`. Keys are scoped per tenant. Concurrent requests with
one key still create exactly one event.

**Pagination.** `?limit=` (1-100, default 50) and `?cursor=` (opaque, taken
from `next_cursor`, which is `null` on the last page).

**Errors** always have the same shape; `code` is stable, `message` is for humans:

```json
{"error": {"code": "invalid_request", "message": "url must include a host", "field": "url", "request_id": "..."}}
```

| Status | `code` |
| --- | --- |
| 400 | `invalid_request` (with `field`) |
| 401 | `unauthorized` |
| 404 | `not_found` (also for another tenant's id) |
| 405 | `method_not_allowed` |
| 413 | `payload_too_large` (endpoints 16 KiB, events 256 KiB) |
| 415 | `unsupported_media_type` |
| 422 | `idempotency_key_reuse` |
| 500 | `internal_error` (details are logged, never returned) |

## Operating

```bash
docker compose run --rm api admin create-key <tenant id> <key name>
docker compose run --rm api admin revoke-key <tenant id> <key id>
```

Without Docker for the Go parts (Go 1.27+, a Postgres you run yourself):

```bash
cp .env.example .env   # then export its variables, or set them in your shell
go run ./cmd/migrate
go run ./cmd/api
```

## Decisions

| Decision | Why |
| --- | --- |
| `/healthz` never touches the database | A database outage must not make an orchestrator restart healthy processes. `/readyz` is the one that checks it. |
| Readiness errors never echo driver messages | They can contain usernames or hostnames. The cause goes to logs only. |
| Config problems are reported together | One failed start tells you everything that is wrong. |
| `Connect` retries with backoff | Containers start in no guaranteed order. |
| Migrations are plain SQL embedded in the binary | The schema ships with the code that needs it. |
| Graceful shutdown drains in-flight requests | A deploy must not cut a request in half. |

## Testing

```bash
docker compose up -d --wait postgres
export TEST_DATABASE_URL='postgres://hookmini:hookmini@127.0.0.1:5432/hookmini?sslmode=disable'
go test ./...
```

Without `TEST_DATABASE_URL` the database tests skip themselves, so `go test
./...` stays green on a bare machine. Store tests run inside rolled-back
transactions; the migration round-trip test (up, down to zero, up) uses a
scratch database it creates and drops.

CI also runs `go test -race`, which needs cgo and is therefore not part of the
local command on Windows.

## Changing the schema

```bash
# 1. add db/migrations/0000N_name.sql (with a working Down) and/or edit db/queries/*.sql
# 2. regenerate the typed code (pinned version, no global install):
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
```

`internal/store` is generated and never edited by hand.

## Roadmap

| Phase | Scope |
| --- | --- |
| 0 | Foundation |
| 1 | Data model and generated queries |
| 2 | API keys, endpoints, event ingestion with idempotency (this) |
| 3 | Queue, worker, retries with backoff and jitter, dead letters |
| 4 | Request signing and SSRF protection |
| 5 | Delivery history, manual replay, auto-disable of failing endpoints |
| 6 | Integration and race tests |
| 7 | OpenAPI, documentation polish |

## License

MIT
