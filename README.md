# Hook Mini

A small webhook delivery service: applications send events, Hook Mini delivers
them to subscriber endpoints with signed requests, retries and a delivery
history. Think Svix, scaled down to something you can read in an afternoon.

> **Status: Phase 0 (foundation).** The project skeleton, configuration,
> migrations, health endpoints and Docker setup exist. The domain model, the
> public API and the delivery engine arrive in later phases; see
> [Roadmap](#roadmap).

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

## Quick start

Requires Docker.

```bash
docker compose up --build
```

```bash
curl localhost:8080/healthz
curl localhost:8080/readyz
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
go test ./...                  # unit tests
TEST_DATABASE_URL=postgres://hookmini:hookmini@127.0.0.1:5432/hookmini?sslmode=disable \
  go test ./internal/database  # needs the compose postgres
```

CI also runs `go test -race`, which needs cgo and is therefore not part of the
local command on Windows.

## Roadmap

| Phase | Scope |
| --- | --- |
| 0 | Foundation (this) |
| 1 | Data model and generated queries |
| 2 | API keys, endpoints, event ingestion with idempotency |
| 3 | Queue, worker, retries with backoff and jitter, dead letters |
| 4 | Request signing and SSRF protection |
| 5 | Delivery history, manual replay, auto-disable of failing endpoints |
| 6 | Integration and race tests |
| 7 | OpenAPI, documentation polish |

## License

MIT
