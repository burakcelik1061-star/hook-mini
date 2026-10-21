# Hook Mini

A small webhook delivery service: applications send events, Hook Mini delivers
them to subscriber endpoints with signed requests, retries and a delivery
history. Think Svix, scaled down to something you can read in an afternoon.

> **Status: Phase 4 (security).** Events are delivered by workers with retries
> and dead-lettering, every request is signed in the
> [Standard Webhooks](https://www.standardwebhooks.com) format, and private,
> loopback and cloud-metadata destinations are refused at connect time. See
> [Roadmap](#roadmap).

## Architecture

```mermaid
flowchart LR
    client[Client app] -->|HTTP| api
    subgraph Hook Mini
        api[api :8080]
        worker[worker x N]
        pg[(PostgreSQL)]
        api --> pg
        worker --> pg
    end
    worker -->|POST webhook| sub[Subscriber endpoint]
```

Two independent processes, one infrastructure dependency:

- **api** serves HTTP. **worker** has no port and only talks to Postgres; run
  as many as you like, they coordinate through row locks and leases.
- **PostgreSQL is the only service.** The job queue is a table read with
  `FOR UPDATE SKIP LOCKED`, not Redis, so there is nothing else to run, back up
  or keep consistent with the database.
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

## Delivery

```mermaid
stateDiagram-v2
    [*] --> pending: event ingested
    pending --> in_flight: claimed (lease)
    in_flight --> succeeded: 2xx
    in_flight --> pending: failure, attempts left (backoff)
    in_flight --> dead: failure on the last attempt
    in_flight --> pending: lease expired, attempts left (reaper)
    in_flight --> dead: lease expired on the last attempt
    in_flight --> dead: endpoint disabled (no request)
```

Each attempt is a `POST` with this body and headers:

```json
{"id": "<event id>", "type": "invoice.paid", "created_at": "2026-10-21T09:00:00Z", "data": {"amount": 42}}
```

`Content-Type: application/json`, `User-Agent: HookMini/1.0`, and the three
Standard Webhooks headers: `webhook-id` (the event id, identical on every retry
so receivers can drop duplicates), `webhook-timestamp` and `webhook-signature`
(see [Security](#security)). A `2xx` is a success; anything else, a timeout or
a connection error is a failure. Redirects are not followed.

**Guarantees.** Delivery is *at least once*.

| Property | How |
| --- | --- |
| A delivery is never handed to two workers at once | One statement: rows selected `FOR UPDATE SKIP LOCKED` in a `MATERIALIZED` CTE (so the locking runs exactly once, which a plan test asserts), then updated with a `status = 'pending'` re-check. |
| A crashed worker loses nothing | Every claim is a lease; any worker's reaper returns expired leases to the queue. |
| A poison message cannot loop forever | The lost attempt counts, so it still ends after `DELIVERY_MAX_ATTEMPTS`. |
| A late result never overwrites a newer one | A fresh `lease_token` per claim fences every write; writes only touch `in_flight` rows; attempt numbers are unique, and the reaper takes the stale worker's number. |
| History is consistent | `attempt_count` always equals the number of attempt rows (asserted after every integration test). |
| Stopping loses no work | No new claims after `SIGTERM`; attempts in flight finish and are recorded, or are cancelled after `SHUTDOWN_TIMEOUT` and retried. |

**Retries.** Waits grow ×4 from `DELIVERY_BACKOFF_INITIAL` up to
`DELIVERY_BACKOFF_MAX` (30s, 2m, 8m, 32m, 2h8m, then 6h; 8 attempts ≈ 15 hours),
with jitter drawn from [base/2, base]. `Retry-After` on a 429/503 can lengthen
a wait, never shorten it. All times come from the database clock.

| Variable | Default | |
| --- | --- | --- |
| `WORKER_CONCURRENCY` | `10` | attempts in flight per worker |
| `WORKER_BATCH_SIZE` | `10` | deliveries claimed per query |
| `WORKER_POLL_INTERVAL` | `500ms` | wait when the queue is empty (worst-case pickup latency) |
| `WORKER_LEASE_DURATION` | `60s` | must be ≥ `DELIVERY_TIMEOUT` + 5s |
| `DELIVERY_TIMEOUT` | `15s` | one attempt, reading the response included |
| `DELIVERY_MAX_ATTEMPTS` | `8` | |
| `DELIVERY_BACKOFF_INITIAL` / `_MAX` | `30s` / `6h` | |

## Security

### Verifying a webhook

Requests are signed per [Standard Webhooks](https://www.standardwebhooks.com):
the signed content is `<webhook-id>.<webhook-timestamp>.<raw body>`, the
signature is `v1,<base64 HMAC-SHA256>` with the endpoint's `whsec_...` secret.
Any Standard Webhooks library verifies it; in Go, `pkg/webhook` does:

```go
v, err := webhook.NewVerifier(os.Getenv("WEBHOOK_SECRET")) // the endpoint's whsec_... secret
// in the handler, with the body exactly as received:
body, _ := io.ReadAll(r.Body)
if err := v.Verify(r.Header, body); err != nil {
	http.Error(w, "invalid signature", http.StatusUnauthorized)
	return
}
```

`Verify` rejects timestamps more than 5 minutes off (replays) and compares in
constant time. Every attempt is signed afresh, so a retry hours later is not
mistaken for a replay. The implementation is tested against the
specification's published example.

**Rotation.** `POST /v1/endpoints/{id}/rotate-secret` returns a new secret
(once). For 24 hours requests carry two signatures, one per secret, so the
receiver can switch whenever it is ready without rejecting a delivery.

### Destinations

A tenant controls endpoint URLs, so without care the worker would send
requests wherever they point: `http://169.254.169.254/` (cloud credentials),
`http://localhost:5432/`, the internal network. `internal/safedial` checks the
**resolved IP of every connection**, after DNS, via `net.Dialer.Control`, so a
hostname that later starts resolving inside (DNS rebinding) is refused too.
Blocked: loopback, private, link-local, CGNAT, multicast, reserved ranges, and
IPv6 forms that embed an IPv4 address (`::ffff:`, NAT64, 6to4, Teredo).
Registration also rejects IP literals and `localhost` early with a clear
error. Proxies from the environment are ignored and redirects never followed.

`ALLOW_PRIVATE_DESTINATIONS=true` lifts this for local development only; api
and worker log a warning when it is on.

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

### Watching deliveries locally

A development receiver logs every webhook it gets. It lives on the Docker
network (a private address), so the demo allows private destinations, and
uses short retries so failures are quick to watch:

```bash
ALLOW_PRIVATE_DESTINATIONS=true DELIVERY_BACKOFF_INITIAL=1s DELIVERY_MAX_ATTEMPTS=3 docker compose --profile demo up -d --build
```

Register `http://receiver:9000/billing` (answers 200) and
`http://receiver:9000/status/503` (always fails) as endpoints. To have the
receiver verify signatures, restart it with an endpoint's secret:

```bash
RECEIVER_SECRETS=whsec_... ALLOW_PRIVATE_DESTINATIONS=true docker compose --profile demo up -d receiver
```

Submit an event, then:

```bash
docker compose logs -f receiver worker
```

## API

All `/v1` routes need `Authorization: Bearer <api key>`. Bodies are JSON.

| Method and path | Purpose |
| --- | --- |
| `POST /v1/endpoints` | Register an endpoint. `201`; the response is the only one that contains `secret`. |
| `GET /v1/endpoints` | List endpoints (newest first, paginated). |
| `GET /v1/endpoints/{id}` | One endpoint. |
| `PATCH /v1/endpoints/{id}` | Change `url` and/or `description`. |
| `POST /v1/endpoints/{id}/rotate-secret` | New signing secret (returned once); the old one keeps signing for 24h. |
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
transactions; the migration round-trip test (up, down to zero, up) and the
worker tests use scratch databases they create and drop (a worker claims every
due delivery in its database, so it must not share one with other tests).

Do not run the compose `worker` against the database the tests use: it would
pick up the deliveries tests create (their URLs use the reserved
`.invalid` domain, so nothing would actually be reached).

`go test -race ./...` needs cgo, i.e. a C compiler (on Windows, for example
`winget install BrechtSanders.WinLibs.POSIX.UCRT` and `CGO_ENABLED=1`). CI
runs it on every push.

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
| 2 | API keys, endpoints, event ingestion with idempotency |
| 3 | Queue, worker, retries with backoff and jitter, dead letters |
| 4 | Request signing and SSRF protection (this) |
| 5 | Delivery history, manual replay, auto-disable of failing endpoints |
| 6 | Integration and race tests |
| 7 | OpenAPI, documentation polish |

## License

MIT
