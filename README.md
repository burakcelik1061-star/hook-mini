# Hook Mini

A small webhook delivery service: applications send events, Hook Mini delivers
them to subscriber endpoints with signed requests, retries and a delivery
history. Think Svix, scaled down to something you can read in an afternoon.

- **At-least-once delivery** with exponential backoff, jitter, `Retry-After`,
  dead-lettering and replay.
- **Standard Webhooks signatures**, secret rotation without dropped deliveries,
  and **SSRF protection** checked on the resolved IP of every connection.
- **Postgres is the only dependency**: the queue is a table read with
  `FOR UPDATE SKIP LOCKED`. Run as many workers as you like.
- **Built to be tested hard**: chaos, contention and fuzz tests, and an OpenAPI
  document that the tests check against the real server.

> **Status: Phase 7 of 8 (documentation).** Everything described here exists and
> is tested; the final security and concurrency review (Phase 8) is still to
> do, and CI has not run yet (see [Known limits](#known-limits)).

**Contents:**
[Quick start](#quick-start) ·
[API](#api) ·
[Architecture](#architecture) ·
[Delivery](#delivery) ·
[Security](#security) ·
[Configuration](#configuration) ·
[Operating](#operating) ·
[Testing](#testing) ·
[Known limits](#known-limits) ·
[Documentation](#documentation)

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

The receiver also answers `/status/<code>` with that status and `/delay/<ms>`
after a pause, which is handy for trying retries and timeouts.

## API

The full contract is [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1, with
the outbound webhook described under `webhooks`), also served by a running
instance at `GET /openapi.yaml`. Tests check it against the real server in
both directions, so it is not a hand-kept approximation (see
[Testing](#testing)).

All `/v1` routes need `Authorization: Bearer <api key>`. Bodies are JSON.

| Method and path | Purpose |
| --- | --- |
| `POST /v1/endpoints` | Register an endpoint. `201`; the response is the only one that contains `secret` (besides rotation). |
| `GET /v1/endpoints` | List endpoints (newest first, paginated). |
| `GET /v1/endpoints/{id}` | One endpoint. |
| `PATCH /v1/endpoints/{id}` | Change `url` and/or `description`. |
| `DELETE /v1/endpoints/{id}` | Delete an endpoint and its delivery history. `204`. |
| `POST /v1/endpoints/{id}/rotate-secret` | New signing secret (returned once); the old one keeps signing for 24h. |
| `POST /v1/endpoints/{id}/disable` / `enable` | Stop or resume deliveries to an endpoint (`enable` resets its failure count). |
| `POST /v1/events` | Submit an event. `201` when new, `200` on an idempotent replay. |
| `GET /v1/events` | List events (newest first, paginated). |
| `GET /v1/events/{id}` | One event with its `delivery_count`. |
| `GET /v1/deliveries` | List deliveries; filter by `endpoint_id`, `event_id`, `status`. |
| `GET /v1/deliveries/{id}` | One delivery with all its attempts. |
| `POST /v1/deliveries/{id}/replay` | Send a dead or succeeded delivery again. `202`. |
| `GET /healthz`, `GET /readyz`, `GET /openapi.yaml` | Liveness, readiness (checks the database), this document. No key needed. |

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
| 401 | `unauthorized` (missing, malformed, unknown and revoked keys look identical) |
| 404 | `not_found` (also for another tenant's id) |
| 405 | `method_not_allowed` |
| 409 | `delivery_not_replayable`, `endpoint_disabled` |
| 413 | `payload_too_large` (endpoints 16 KiB, events 256 KiB) |
| 415 | `unsupported_media_type` |
| 422 | `idempotency_key_reuse` |
| 500 | `internal_error` (details are logged with the `request_id`, never returned) |

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
- **migrate** applies the embedded SQL migrations under an advisory lock (so
  replicas starting together take turns) and exits; compose starts the api and
  worker only after it succeeds.
- **admin** creates tenants and API keys; there is deliberately no HTTP route
  for that.

Code layout: `cmd/` (binaries), `internal/httpapi` (HTTP only),
`internal/service` (business rules), `internal/store` (generated by sqlc),
`internal/delivery` (the worker), `internal/safedial` (SSRF policy),
`pkg/webhook` (signing and verification, importable by receivers),
`db/migrations` and `db/queries` (plain SQL), `api/` (OpenAPI).

### Data model

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
  endpoints, locked `FOR KEY SHARE` so a concurrent endpoint deletion cannot
  break it) and idempotent: `UNIQUE (event_id, endpoint_id)`.
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
    dead --> pending: replay
    succeeded --> pending: replay
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
| Stopping loses no work | No new claims after `SIGTERM`; attempts in flight finish and are recorded. One still running after `SHUTDOWN_TIMEOUT` is cancelled, recorded, and queued again at once without using up a retry or counting against the endpoint (the worker cut it short, not the endpoint). Compose gives the processes 30s before `SIGKILL`. |

**Retries.** Waits grow ×4 from `DELIVERY_BACKOFF_INITIAL` up to
`DELIVERY_BACKOFF_MAX` (30s, 2m, 8m, 32m, 2h8m, then 6h; 8 attempts ≈ 15 hours),
with jitter drawn from [base/2, base]. `Retry-After` on a 429/503 can lengthen
a wait, never shorten it. All times come from the database clock.

### Failing endpoints and replay

- **Inspect.** `GET /v1/deliveries?status=dead&endpoint_id=...` lists what
  failed; `GET /v1/deliveries/{id}` shows every attempt (status code, duration,
  the start of the response body, the error).
- **Replay.** `POST /v1/deliveries/{id}/replay` sends a `dead` or `succeeded`
  delivery again with a fresh retry budget. The attempt history is kept and
  keeps counting up (a `round_base` column tracks the budget per round). Two
  simultaneous replays queue it once; the second gets `409`.
- **Auto-disable.** After `ENDPOINT_AUTO_DISABLE_AFTER` failed attempts in a
  row (any success resets the count) the worker disables the endpoint. Its
  pending deliveries end as `dead` with the reason, so nothing is lost: fix
  the endpoint, `POST /v1/endpoints/{id}/enable`, and replay them. Replaying
  to a disabled endpoint is refused with `409 endpoint_disabled`.

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
Blocked: loopback, private, link-local, CGNAT, multicast, reserved ranges,
Azure's WireServer, and IPv6 forms that embed an IPv4 address (`::ffff:`,
NAT64, 6to4, Teredo). IPv6 is judged by an allowlist: only global unicast
(`2000::/3`) is reachable. Registration also rejects IP literals and
`localhost` early with a clear error. Proxies from the environment are ignored
and redirects never followed.

The attempt history a tenant reads says *why* a connection failed (`dns: no
such host`, `connect: connection refused`, `blocked: ... (private network)`)
but never *where*: Go's error texts name resolved addresses, the worker's own
address and the DNS server, which would let a tenant map the internal network.
The full error goes to the worker's log only.

`ALLOW_PRIVATE_DESTINATIONS=true` lifts this for local development only; api
and worker log a warning when it is on.

### Other measures

API keys and signing secrets never appear in logs or in any response except the
one that creates them; responses are built from explicit types, never from
database rows. Authentication failures are indistinguishable from each other.
Another tenant's resource id is a `404`, as if it did not exist. Request bodies are
size-limited, strictly decoded (unknown fields are rejected) and input the
database cannot store is a `400`, never a `500`.

The `X-Request-Id` on every response is generated by the server; one sent by
the client is ignored, so it can neither flood the logs nor pose as another
request. A panic in a handler still answers with the documented error
envelope, and the stack is logged with the request ID. Every database session
has `statement_timeout` (30s), `lock_timeout` (10s) and
`idle_in_transaction_session_timeout` (60s), so a query stuck behind a lock
fails instead of holding a pool connection indefinitely. Response headers from
endpoints are capped at 64 KB, and `pkg/webhook` accepts at most 16 signatures
per request.

## Configuration

All settings are environment variables; invalid or inconsistent values are all
reported together at startup. `.env.example` lists them.

| Variable | Default | Used by | Meaning |
| --- | --- | --- | --- |
| `DATABASE_URL` | (required) | all | `postgres://` connection string |
| `HTTP_ADDR` | `:8080` | api | listen address |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | all | `json` for production, `text` for a terminal |
| `SHUTDOWN_TIMEOUT` | `20s` | api, worker | how long to wait for work in flight on `SIGTERM`; must be ≥ `DELIVERY_TIMEOUT` + 5s |
| `ALLOW_PRIVATE_DESTINATIONS` | `false` | api, worker | **development only**: allow loopback/private targets |
| `WORKER_ID` | generated | worker | name in leases and logs |
| `WORKER_CONCURRENCY` | `10` | worker | attempts in flight per worker |
| `WORKER_BATCH_SIZE` | `10` | worker | deliveries claimed per query |
| `WORKER_POLL_INTERVAL` | `500ms` | worker | wait when the queue is empty (worst-case pickup latency) |
| `WORKER_LEASE_DURATION` | `60s` | worker | must be ≥ `DELIVERY_TIMEOUT` + 5s |
| `DELIVERY_TIMEOUT` | `15s` | worker | one attempt, reading the response included |
| `DELIVERY_MAX_ATTEMPTS` | `8` | worker | per round (a replay starts a new round) |
| `DELIVERY_BACKOFF_INITIAL` / `_MAX` | `30s` / `6h` | worker | retry waits |
| `ENDPOINT_AUTO_DISABLE_AFTER` | `100` | worker | consecutive failed attempts before an endpoint is disabled; `0` = never |
| `RECEIVER_ADDR`, `RECEIVER_SECRETS` | `:9000`, empty | demo receiver | address; secrets to verify signatures with |

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

Scale delivery by running more workers (`docker compose up -d --scale worker=3`).
Stopping a worker (`SIGTERM`) is safe at any time; killing one (`SIGKILL`) is
too, and its work is picked up by another after its lease expires.

### Changing the schema

```bash
# 1. add db/migrations/0000N_name.sql (with a working Down) and/or edit db/queries/*.sql
# 2. regenerate the typed code (pinned version, no global install):
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
```

`internal/store` is generated and never edited by hand.

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

### What is tested beyond the unit tests

| Kind | Where | What it proves |
| --- | --- | --- |
| **Contract** | `internal/httpapi/contract_test.go` | `api/openapi.yaml` and the server agree in both directions: every route is documented and every documented operation is served; every response the server gives (status, headers, JSON body) matches the schema, an undocumented field or status is a failure, and so is a documented response that never occurs; the webhook a worker sends matches the document too. |
| **Chaos** | `internal/delivery/chaos_test.go` | 3 workers restarted at random, "crashed" workers that claim and vanish, failing/hanging/slow/rate-limiting endpoints, producers with repeating idempotency keys, an operator replaying, toggling, rotating and deleting, all at once. Restarts interrupt attempts mid-flight. Afterwards: every delivery is final, attempt history consistent, no retry budget exceeded and no interrupted attempt counted against one, nothing crossed tenants, every request validly signed, every "succeeded" delivery really arrived. A run that did not produce each kind of event it claims to test fails. The seed is printed on failure. |
| **Contention** | `internal/delivery/stress_test.go` | 16 concurrent claimers over 400 deliveries: each handed out exactly once, each with its own lease; 8 concurrent reapers reclaim each expired lease once. |
| **Fuzzing** | `*/fuzz_test.go` | Validators agree with Postgres (bad input is a `400`, never a `500`); the SSRF policy is consistent across IPv4/IPv6 encodings; signatures round-trip and any tampering is detected; parsers never panic. |
| **Plan test** | `internal/store/plan_test.go` | The claim query takes its locks exactly once (`EXPLAIN ANALYZE`). |
| **Mutation checks** | run by hand | Each safety property was removed from the code (or the OpenAPI document was made wrong) to confirm a test fails. Results are in `docs/`. |

Fuzzing runs as ordinary unit tests over the seed inputs; to fuzz for real
(the service targets need `TEST_DATABASE_URL`):

```bash
go test -run '^$' -fuzz '^FuzzIngestEvent$' -fuzztime 60s ./internal/service
go test -run '^$' -fuzz '^FuzzCheckConsistency$' -fuzztime 60s ./internal/safedial
```

Inputs that once failed are kept under `testdata/fuzz/` and rerun on every
`go test`.

## Design decisions

| Decision | Why |
| --- | --- |
| Postgres as the queue, no Redis | One thing to run and back up; the queue and the data commit in the same transaction, so an accepted event can never lose its deliveries. |
| At-least-once, not exactly-once | Exactly-once over HTTP is not achievable; a stable `webhook-id` lets receivers deduplicate. |
| Leases with fencing tokens | A worker that is slow or partitioned must not overwrite the result of the worker that took its job over. |
| The database enforces invariants | Tenant isolation, lease consistency and the attempt history are constraints, so a bug in one query cannot corrupt them. |
| SSRF check at connect time, on the resolved IP | Validating the URL once is not enough: DNS can change, and a name can resolve to several addresses. |
| `/healthz` never touches the database | A database outage must not make an orchestrator restart healthy processes. `/readyz` is the one that checks it. |
| Errors never echo internals | Driver messages can contain usernames or hostnames; the cause goes to the logs with the request ID. |
| Config problems are reported together | One failed start tells you everything that is wrong. |
| Migrations are plain SQL embedded in the binary | The schema ships with the code that needs it. |

## Known limits

Chosen limits and gaps, so nothing here is a surprise:

- **Signing secrets are stored in plain text** in the database: HMAC needs the
  raw value. Encryption at rest (and a key-management story) is not done.
- **No rate limiting** on the API, and no per-tenant or per-endpoint fairness:
  one tenant sending a flood can occupy the queue.
- **No retention**: deliveries, attempts and events are kept forever.
- **Replay is one delivery at a time**; there is no "replay everything that
  failed for this endpoint" call.
- **Workers poll** (500 ms by default) instead of using `LISTEN/NOTIFY`.
- **Endpoint ports are not restricted**: a public IP on any port is allowed.
- **Single database, no failover story**; the chaos test simulates crashing
  workers, not network partitions or a Postgres restart.
- **No load testing**: no throughput or latency numbers are claimed.
- **Interrupted attempts are free**: one the worker cuts short while stopping
  does not use up a retry, so a worker restarted mid-attempt over and over
  could retry a delivery indefinitely. A crash or `kill -9` still counts (as a
  lost lease).
- **A tenant can tell whether a host name resolves** and to what kind of
  address (`blocked: ... (private network)`), though never the address itself.
- **Platform**: developed and tested on Windows with Docker; **CI has not run
  yet** (the repository has not been pushed), so the workflow file is
  unverified.

## Documentation

Design notes, in Turkish, one per phase: why each decision was made, what was
tried and what the tests found.

| File | Topic |
| --- | --- |
| [`docs/00-zemin.md`](docs/00-zemin.md) | Foundation: config, logging, health, shutdown |
| [`docs/01-veri-modeli.md`](docs/01-veri-modeli.md) | Schema, constraints, indexes, sqlc |
| [`docs/02-api.md`](docs/02-api.md) | Authentication, endpoints, ingestion, idempotency |
| [`docs/03-kuyruk-ve-worker.md`](docs/03-kuyruk-ve-worker.md) | Queue, leases, fencing, retries, shutdown |
| [`docs/04-imza-ve-ssrf.md`](docs/04-imza-ve-ssrf.md) | Signatures, rotation, SSRF policy |
| [`docs/05-gecmis-ve-yeniden-gonderim.md`](docs/05-gecmis-ve-yeniden-gonderim.md) | History, replay, auto-disable |
| [`docs/06-testler.md`](docs/06-testler.md) | Chaos, contention, fuzz; the four bugs they found |
| [`docs/07-openapi.md`](docs/07-openapi.md) | The OpenAPI document and how it is kept honest |
| [`docs/08-son-inceleme.md`](docs/08-son-inceleme.md) | Final security and concurrency review: 16 findings, what changed, what the chaos test taught |
| [`api/openapi.yaml`](api/openapi.yaml) | The API contract |

## Roadmap

| Phase | Scope | |
| --- | --- | --- |
| 0 | Foundation | done |
| 1 | Data model and generated queries | done |
| 2 | API keys, endpoints, event ingestion with idempotency | done |
| 3 | Queue, worker, retries with backoff and jitter, dead letters | done |
| 4 | Request signing and SSRF protection | done |
| 5 | Delivery history, manual replay, auto-disable of failing endpoints | done |
| 6 | Integration, chaos and fuzz tests | done |
| 7 | OpenAPI, documentation polish | done |
| 8 | Final security and concurrency review | done |

## License

MIT
