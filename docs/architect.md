# Architecture — shorten-link

A URL shortener: users sign up, turn long URLs into short codes like
`https://short.example.com/R7dwPCPmUa`, and anyone who opens the short link is
redirected to the original URL. Every click is counted in the background.

This document explains how the pieces fit together. For *what* the system must
do, see [spec.md](spec.md). For *how to work on it*, see
[CONTRIBUTING.md](../CONTRIBUTING.md).

---

## 1. The big picture

```
                         Internet
                            │
                            ▼
                  ┌───────────────────┐
                  │       nginx       │  TLS, coarse rate limit (100 r/s/IP),
                  │                   │  bad user-agent block, 2 MB body cap
                  └─────────┬─────────┘
                            │  sets X-Real-IP
                            ▼
                  ┌───────────────────┐
                  │   server (Go)     │  HTTP API + redirects
                  │   cmd/server      │
                  └──┬──────┬──────┬──┘
                     │      │      │ publish "link.clicked"
          read/write │      │      ▼
                     │      │   ┌──────────┐      ┌──────────────┐
                     │      │   │ RabbitMQ │ ───▶ │ worker (Go)  │
                     │      │   └──────────┘      │ cmd/worker   │
                     │      │                     └──────┬───────┘
                     ▼      ▼                            │ batch UPDATE clicks
               ┌───────┐ ┌───────┐                       │
               │ MySQL │ │ Redis │ ◀─────────────────────┘ (MySQL)
               └───────┘ └───────┘
```

There are **two programs**, built from the same codebase:

| Program | What it does | Talks to |
|---|---|---|
| **server** (`cmd/server`) | Serves the REST API, the redirects and `/docs` | MySQL, Redis, RabbitMQ (publish only) |
| **worker** (`cmd/worker`) | Reads click events and adds them to each link's counter | RabbitMQ (consume), MySQL |

And **three backing services**:

| Service | Why it exists |
|---|---|
| **MySQL** | Source of truth: users and links |
| **Redis** | Fast lookups and short-lived state: link cache, refresh tokens, login lockout counters, rate-limit windows |
| **RabbitMQ** | Decouples "someone clicked" from "write to the database", so redirects stay fast |

---

## 2. What happens on a redirect (the hot path)

This is the request that matters most: it must be fast and must keep working
even if parts of the system are slow.

```
GET /R7dwPCPmUa
  │
  ├─ middleware chain (see §4)
  ├─ rate limit: 60 requests/minute per client IP (Redis)
  │
  ├─ LinkService.Resolve("R7dwPCPmUa")
  │     ├─ Redis GET sl:R7dwPCPmUa ── hit ──▶ URL
  │     └─ miss ──▶ MySQL SELECT … WHERE short_code = ?
  │                   ├─ not found or expired ──▶ 404
  │                   └─ found ──▶ cache it in Redis (TTL ≤ 24h, never past expiry)
  │
  ├─ fire-and-forget: publish {"code":"R7dwPCPmUa"} to RabbitMQ (2s timeout)
  │
  └─ 302 Found, Location: <original URL>
```

Key ideas:

- **Cache first.** Most redirects never touch MySQL.
- **Clicks are async.** The redirect does not wait for the click to be counted.
  If RabbitMQ is down, the redirect still works; that click is lost.
- **Expiry is enforced twice:** the cache TTL never outlives `expires_at`, and
  the database path checks `expires_at` before redirecting.

## 3. What happens when a click is counted

```
RabbitMQ queue "clicks"
  │
  ▼
worker ClickConsumer
  ├─ collects up to 500 events, or whatever arrived in 2 seconds
  ├─ sums them per code: {"R7dwPCPmUa": 37, "Kp3xQ9aZ1b": 4}
  ├─ one MySQL transaction: UPDATE links SET clicks = clicks + ? …
  ├─ success ──▶ ack all messages
  └─ failure ──▶ retry once; second failure ──▶ dead-letter queue "clicks.dlq"
                                                (logged by DLQConsumer)
```

Batching turns thousands of clicks per second into a few database writes.

---

## 4. Inside the server

### Layers

The code follows a simple three-layer layout. Each layer only knows about the
one below it, through small interfaces.

```
 HTTP request
     │
     ▼
┌─────────────────────────────┐
│ middleware/                 │  cross-cutting concerns (auth, limits, headers)
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ handler/                    │  parse JSON, check input shape, map errors → HTTP status
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ service/                    │  business rules: validation, SSRF guard, expiry,
│                             │  lockout, token rotation. Defines the interfaces
│                             │  it needs (LinkRepository, LinkCache, TokenCache…)
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ repository/  messaging/     │  MySQL, Redis, RabbitMQ implementations
└─────────────────────────────┘
```

**Interfaces live where they are used** (in `service/` and `handler/`), not
where they are implemented. That is why tests can swap in simple mocks.

### Middleware chain

Every request passes through these, outermost first:

| # | Middleware | Purpose |
|---|---|---|
| 1 | `http.TimeoutHandler` | Abort any request that takes longer than 10s |
| 2 | `SecureHeaders` | HSTS, CSP, X-Frame-Options, nosniff, etc. |
| 3 | `CORS` | Allow only origins listed in `ALLOWED_ORIGINS` |
| 4 | `JWTAuth` | If a valid `Authorization: Bearer` token is present, put the user ID in the request context |
| 5 | `RequestID` | Reuse or create `X-Request-ID`; attach it to the logger |
| 6 | `MaxBodySize` | Reject bodies over 1 MB (413) |
| 7 | `RateLimit` | Per-IP sliding window on redirects, `POST /api/links` and `/api/auth/*` (see spec §4) |
| 8 | `RequireAuth` | Only on protected routes; returns 401 if step 4 found no user |

### Routes

| Method | Path | Auth | What it does |
|---|---|---|---|
| GET | `/{code}` | — | Redirect to the original URL |
| POST | `/api/links` | ✔ | Create a short link (optional custom `alias`) |
| GET | `/api/links?cursor=` | ✔ | List your links, 20 per page |
| GET | `/api/links/availability?alias=` | ✔ | Check whether a custom alias can be used |
| DELETE | `/api/links/{code}` | ✔ | Delete one of your links |
| POST | `/api/auth/register` | — | Create account, returns tokens |
| POST | `/api/auth/login` | — | Log in, returns tokens |
| POST | `/api/auth/refresh` | — | Swap a refresh token for a new pair |
| POST | `/api/auth/forgot-password` | — | Email a password reset link (always 202) |
| POST | `/api/auth/reset-password` | — | Set a new password with the link's token; ends all sessions |
| GET | `/healthz` | — | Pings MySQL, Redis, RabbitMQ |
| GET | `/docs`, `/api/openapi.yaml` | — | Swagger UI and the raw spec |

---

## 5. Modules, one by one

Each package has its own page with the details in [modules/](modules/README.md).

| Package | Responsibility | Read this first |
|---|---|---|
| `cmd/server` | Wires everything together, builds the middleware chain, graceful shutdown | `main.go` |
| `cmd/worker` | Starts the click and DLQ consumers, shuts down cleanly | `main.go` |
| `config` | Reads env vars (`envconfig`), opens MySQL/Redis/RabbitMQ, loads RSA keys | `config.go` |
| `internal/handler` | HTTP handlers for links, auth, health, docs | `link.go` |
| `internal/service` | Business logic and the interfaces it depends on | `link_service.go`, `auth_service.go` |
| `internal/repository` | MySQL (`*_mysql.go`) and Redis (`*_redis.go`) implementations | `link_mysql.go` |
| `internal/messaging` | RabbitMQ topology, publisher, click and DLQ consumers | `setup.go` |
| `internal/mail` | Password reset email over SMTP, or to the log when `SMTP_HOST` is empty | `mail.go` |
| `internal/middleware` | Everything in the middleware table above | `ratelimit.go` |
| `internal/ctxkey` | Typed context key for the authenticated user ID | — |
| `internal/logger` | zap logger, stored in and read from the request context | — |
| `pkg/idgen` | Snowflake ID generator + Base62 encoder (IDs become short codes) | `idgen.go` |
| `api` | Embedded `openapi.yaml` | — |
| `deploy/migrations` | SQL schema (golang-migrate format) | — |
| `deploy/nginx` | Reverse proxy config (template; `${DOMAIN}` filled in at container start) | — |
| `Dockerfile`, `docker-compose.prod.yml` | Production images and single-VPS stack; see [deploy.md](deploy.md) | — |
| `.github/workflows/ci.yml` | Tests (with race detector), lint, govulncheck, image build | — |

---

## 6. How short codes are made

No random strings: uniqueness comes from the ID itself.

1. `idgen` produces a **Snowflake ID**: a 64-bit number made of
   *milliseconds since 2024-01-01* + *node ID (0–1023)* + *sequence (0–4095)*.
2. That number is written in **Base62** (`0-9A-Za-z`), giving a 10-character code today (11 from about 2030).

Codes are therefore unique as long as **every server instance has a different
`NODE_ID`**. Two instances with the same node ID can generate the same code.

A user can instead pick a **custom alias** (3–16 of `A-Za-z0-9_-`). Aliases
share the `short_code` column, which compares bytes (`utf8mb4_bin`), so codes
are case-sensitive. An alias may take a code the generator later produces;
`Shorten` then gets `ErrCodeTaken` from the unique key and draws a new ID, up
to 3 attempts.

---

## 7. Authentication

- **Access token:** RS256 JWT, 15 minutes, `sub` = user ID. Verified with the
  public key on every request, no database lookup.
- **Refresh token:** RS256 JWT, 7 days, carries a random `jti`. The `jti` is
  stored in Redis (`rt:<jti>`). Refreshing atomically deletes it (`GETDEL`) and
  issues a new pair, so each refresh token works exactly once.
- **Passwords:** bcrypt cost 12, 8–72 bytes. Emails are trimmed and lowercased.
- **Lockout:** 5 failed logins for an email lock it for 15 minutes. Unknown
  emails behave the same way (same timing, same responses) so attackers cannot
  tell which emails are registered.

---

## 8. Where data lives

### MySQL tables

| Table | Used? | Contents |
|---|---|---|
| `users` | ✔ | id, email (unique), password_hash, plan, quota_daily |
| `links` | ✔ | id, user_id, short_code (unique), original_url, clicks, expires_at |
| `analytics_daily` | not yet | Per-day click counts (planned) |
| `audit_log` | not yet | Planned audit trail |

### Redis keys

| Key | Value | TTL |
|---|---|---|
| `sl:<code>` | original URL | ≤ 24h, never past expiry |
| `rt:<jti>` | user ID | 7 days |
| `urt:<userID>` | set of the user's refresh token IDs | 7 days |
| `urv:<userID>` | time of the user's last "revoke all sessions" | 7 days |
| `lockout:<email>` | failed login count | 15 min from first failure |
| `prt:<sha256>` | user ID (password reset token) | 30 min |
| `prt-user:<userID>` | hash of the current reset token | 30 min |
| `prt-cooldown:<email>` | reset mail sent recently | 60 s |
| `rl:<action>:<ip>` | sorted set of request timestamps | window + 1s |

### RabbitMQ

| Name | Type | Notes |
|---|---|---|
| `shortlink` | topic exchange | server publishes `link.created`, `link.clicked` |
| `clicks` | queue | bound to `link.clicked`; dead-letters to `shortlink.dlx` |
| `shortlink.dlx` | direct exchange | — |
| `clicks.dlq` | queue | failed click batches end up here |

`link.created` has no consumer yet; it is published for future use.

---

## 9. Failure behaviour

| If this is down… | Redirects | Create/list links | Login | Clicks counted |
|---|---|---|---|---|
| **Redis** | work (from MySQL, no rate limit) | work | **fail** (tokens live in Redis) | yes |
| **MySQL** | work for cached links only | fail | fail | no, retried then DLQ |
| **RabbitMQ** | work | work | work | **lost** for that period |
| **worker** | work | work | work | delayed (queue grows) |

`/healthz` returns 503 if any of MySQL, Redis or RabbitMQ is unreachable.

The server does not need RabbitMQ to start or keep running: the publisher
re-dials every 2s in the background and re-declares the exchange and queues
when it reconnects. The worker exits when it loses RabbitMQ and is restarted
by Docker.
