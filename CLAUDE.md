# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

This is a **pre-implementation** Go URL shortener service. Only the implementation plan (`shortlink-implementation-plan.md`) exists so far — no code has been written.

## Planned commands (Makefile targets)

Once the Makefile is created:

```
make run          # run the API server
make build        # build binaries
make test         # run all tests
make migrate      # run database migrations
make lint         # run golangci-lint
make docker-up    # start MySQL + Redis + RabbitMQ via docker-compose
```

## Architecture

```
Handler → Service → Repository → DB / Cache
                 ↓
           Messaging → RabbitMQ
```

**Dependency rule:** each layer imports only the layer directly below it. Interfaces are defined at the *consumer* site (in `service/`), not at the implementation site.

### Planned directory structure

```
cmd/
  server/    # API binary
  worker/    # analytics consumer binary (scales independently from API)
internal/
  handler/   # HTTP handlers (stdlib net/http, no framework)
  service/   # business logic; defines repository interfaces
  messaging/ # RabbitMQ publisher + consumers
pkg/
  idgen/     # Snowflake ID generator + Base62 encoder
config/      # envconfig struct
deploy/      # Dockerfile, K8s manifests
```

### Critical paths

**Redirect (hot path, ~95% of traffic):**
- Redis lookup (~0.3ms) → on miss: MySQL lookup (~3ms) → warm cache → async publish click event → 302 redirect
- Never block the redirect on analytics

**Shorten (write path, ~5% of traffic):**
- `idgen.NextID()` (local, no network) → MySQL write → Redis warm → async publish event → 201 response

**Analytics consumer (background, fully decoupled):**
- RabbitMQ → batch 500 events OR flush every 2s → single `BatchUpdateClicks()` per short_code per flush

## Tech stack

| Layer | Choice |
|---|---|
| Language | Go 1.22 |
| HTTP | stdlib `net/http` (no framework) |
| ORM | `sqlx` |
| Cache | Redis 7 + `go-redis/v9` |
| Queue | RabbitMQ 3.13 + `amqp091-go` |
| Config | `envconfig` |
| Logger | `zap` (structured JSON, injected via context) |
| Migration | `golang-migrate` (versioned SQL files) |
| Lint | `golangci-lint` (`errcheck`, `staticcheck`, `contextcheck`) |

## Key implementation details

- **IDs:** Snowflake (not `AUTO_INCREMENT`) — API pods generate IDs locally without a DB round-trip. Base62-encoded to 7–8 char short codes.
- **Cache key prefix:** `sl:` in Redis. Pool size: 100 connections.
- **MySQL pool:** 50 open / 20 idle connections.
- **Rate limits:** 60 req/min per IP for redirects, 10 req/min for link creation (Redis sliding window).
- **JWT:** RS256, access token 15min TTL, refresh token 7d stored in Redis with rotation.
- **Auth:** `GET /{code}` is a public endpoint — skip JWT middleware there.
- **Analytics table** (`analytics_daily`): `clicks` is a batch-updated counter, never written per-redirect.
- **Startup:** ping DB, Redis, and RabbitMQ on startup — fail fast if any are down.
- **Worker binary:** graceful shutdown via `os.Signal` → context cancel → drain in-flight messages before exit.

## DB schema summary

| Table | Key index |
|---|---|
| `users` | `UNIQUE(email)` |
| `links` | `UNIQUE(short_code)` — primary hot index |
| `analytics_daily` | `(link_id, stat_date)` |
| `audit_log` | `(user_id, created_at)` |

## Testing approach

- Repository layer: integration tests with `testcontainers` (real MySQL + Redis, not mocks)
- Service layer: unit tests with mock repositories
- Target: 100% branch coverage on `Resolve()` cache hit/miss paths; CI fails if overall coverage < 70%
