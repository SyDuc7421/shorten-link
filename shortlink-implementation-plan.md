# Short-link backend — implementation plan (Tier 1)

**Stack:** Go 1.22 · MySQL 8 · Redis 7 · RabbitMQ 3.13  
**Target:** 10K–100K req/s · Single region · ~18 days

---

## Phase 1 — Project bootstrap (Day 1–2)

### Repo & tooling

- [ ] Init Go module + folder structure
  - `go mod init`, create `cmd/`, `internal/`, `pkg/`, `config/`, `deploy/` dirs
- [ ] Setup Makefile
  - targets: `run`, `build`, `test`, `migrate`, `lint`, `docker-up`
- [ ] Add `.env.example` + config struct
  - `envconfig` package, all required fields with defaults
- [ ] Setup zap logger with request-id middleware
  - structured JSON logs, inject logger into context

### Local infra

- [ ] `docker-compose.yml` with MySQL + Redis + RabbitMQ
  - pin versions: MySQL 8, Redis 7, RabbitMQ 3.13-management
- [ ] Write first migration files
  - `001_create_users.sql`, `002_create_links.sql`, `003_create_analytics.sql`
- [ ] Verify connections in `main.go`
  - ping DB, Redis, RabbitMQ on startup — fail fast if any down

---

## Phase 2 — Core data layer (Day 3–5)

### pkg/idgen

- [ ] Implement Snowflake ID generator
  - timestamp + nodeID + sequence, mutex-safe, 4096 IDs/ms per node
- [ ] Implement Base62 encoder
  - `ToBase62(int64) string` — 7–8 char output from Snowflake IDs
- [ ] Unit test: uniqueness + ordering
  - generate 100K IDs concurrently, assert no duplicates

### repository/

- [ ] Define `LinkRepository` interface in `service/`
  - `Create`, `GetByCode`, `Delete`, `ListByUser` — interface at consumer site
- [ ] Implement `link_mysql.go`
  - sqlx, prepared statements, connection pool config (50 open, 20 idle)
- [ ] Implement `link_redis.go` (cache)
  - Get/Set/Delete with TTL, key prefix `sl:`, pool 100 conns
- [ ] Write integration tests with testcontainers
  - spin real MySQL + Redis in test, assert round-trip

---

## Phase 3 — Business logic + HTTP (Day 6–9)

### service/link.go

- [ ] `Shorten()`: validate URL, generate code, persist + warm cache
  - reject non-http/https, check user quota, set Redis immediately after MySQL write
- [ ] `Resolve()`: Redis-first, fallback MySQL, async publish click
  - `go func()` publish — never block redirect on analytics
- [ ] `Delete()` + `ListByUser()`
  - delete from both MySQL and Redis cache atomically
- [ ] Unit test with mock repositories
  - 100% branch coverage on `Resolve()` cache hit/miss paths

### handler/

- [ ] `POST /api/links` — shorten endpoint
  - parse JSON body, call service, return `{short_url, code, expires_at}`
- [ ] `GET /{code}` — redirect endpoint
  - 302 redirect, extract IP + User-Agent for analytics event
- [ ] `DELETE /api/links/{code}`
  - auth check: only owner can delete
- [ ] `GET /api/links` — list user's links
  - pagination: cursor-based on `id`, limit 20
- [ ] Rate limit middleware (Redis sliding window)
  - 60 req/min per IP for redirect, 10 req/min for create

---

## Phase 4 — Messaging & analytics (Day 10–12)

### messaging/

- [ ] RabbitMQ connection + setup (exchange, queues, DLQ)
  - declare all resources idempotently, auto-reconnect goroutine
- [ ] Publisher: `Publish(ctx, routingKey, payload)`
  - persistent delivery mode, JSON serialization
- [ ] Click consumer: batch 500 or flush every 2s
  - group by `short_code`, single UPDATE per code per flush, manual ACK
- [ ] DLQ consumer: log + alert on failed messages
  - retry 3x with backoff before dropping to dead queue

### cmd/worker/

- [ ] Worker binary: start consumers independently
  - separate from API binary — scale consumer pods without scaling API
- [ ] Graceful shutdown: drain in-flight messages
  - `os.Signal` → context cancel → wait for consumer flush before exit

---

## Phase 5 — Auth + user management (Day 13–15)

### service/auth.go

- [ ] Register: hash password with bcrypt (cost 12)
  - check email uniqueness, return JWT on success
- [ ] Login: verify bcrypt + issue JWT (RS256)
  - access token 15min TTL, refresh token 7d stored in Redis
- [ ] Refresh token rotation
  - invalidate old token on use, detect reuse as compromise signal
- [ ] Auth middleware: verify JWT, inject `userID` to context
  - skip for `GET /{code}` redirect — public endpoint

---

## Phase 6 — Observability + deploy (Day 16–18)

### Observability

- [ ] Prometheus metrics: redirect latency p99, cache hit rate
  - histogram for redirect duration, counter for cache hit/miss
- [ ] Health check endpoints: `/healthz` + `/readyz`
  - liveness vs readiness — `/readyz` checks DB + Redis connectivity
- [ ] Request ID propagation (`X-Request-ID` header)
  - generate if absent, log in every request, return in response

### Deploy

- [ ] Multi-stage Dockerfile (build + scratch/alpine)
  - final image < 20MB, non-root user, no shell
- [ ] K8s manifests: Deployment, Service, HPA, ConfigMap
  - HPA on CPU 60% + RPS, min 2 replicas, rolling update strategy
- [ ] CI pipeline: test → lint → build → push → deploy
  - GitHub Actions, fail PR if test coverage < 70%

---

## Architecture reference

```
Handler → Service → Repository → DB / Cache
                 ↓
           Messaging → RabbitMQ
```

**Dependency rule:** each layer only imports the layer directly below it.  
Interfaces are defined at the *consumer* site (in `service/`), not at the implementation site.

### Critical paths

**Redirect (hot path ~95% of traffic):**
```
GET /{code}
  → Redis.Get(code)             # ~0.3ms — HIT: return immediately
  → MySQL.GetByCode(code)       # ~3ms   — cache miss only
  → Redis.Set(code, url)        # warm cache
  → go mq.Publish(ClickEvent)   # async, non-blocking
  → 302 redirect
```

**Shorten (write path ~5% of traffic):**
```
POST /api/links
  → idgen.NextID()              # local, no network call
  → MySQL.CreateLink(link)      # persist first
  → Redis.Set(code, url)        # warm cache immediately
  → go mq.Publish(CreatedEvent)
  → 201 {short_url, code}
```

**Analytics consumer (background, fully decoupled):**
```
RabbitMQ → consumer.Start()
  → batch 500 events OR every 2s
  → group by short_code
  → MySQL.BatchUpdateClicks()   # 1 UPDATE per code, not per event
```

---

## DB schema (summary)

| Table | Purpose | Key index |
|---|---|---|
| `users` | Auth, plan/quota | `UNIQUE(email)` |
| `links` | Persistent short→long mapping | `UNIQUE(short_code)` — hot index |
| `analytics_daily` | Aggregated click stats per day | `(link_id, stat_date)` |
| `audit_log` | User action history | `(user_id, created_at)` |

`links.id` uses Snowflake (not `AUTO_INCREMENT`) so API pods generate IDs locally without a DB round-trip.  
`links.clicks` is a fast-read counter updated in batch via the analytics consumer — never written per-redirect.

---

## Tech stack

| Layer | Choice | Reason |
|---|---|---|
| Language | Go 1.22 | Goroutine model, stdlib `net/http` with `PathValue` |
| HTTP | stdlib `net/http` | No framework needed for this surface area |
| ORM | `sqlx` | Reduces scan boilerplate, stays close to SQL |
| Cache | Redis 7 + `go-redis/v9` | Built-in connection pool, cluster-ready for tier 2 |
| Queue | RabbitMQ 3.13 + `amqp091-go` | Durable messages, DLQ, sufficient for ≤50K events/s |
| Config | `envconfig` | 12-factor, struct tags, validates required fields |
| Logger | `zap` | Zero-alloc, structured JSON, context injection |
| Migration | `golang-migrate` | Versioned SQL files, CLI + programmatic |
| Lint | `golangci-lint` | `errcheck`, `staticcheck`, `contextcheck` |
