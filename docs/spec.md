# System Specification — shorten-link

What the system must do, and how well the current code meets it.

- **Status legend:** ✅ met · ⚠️ partial · ❌ not met
- Last audited: 2026-10-10, branch `fix/review-issues`
- Architecture: [architect.md](architect.md) · Roadmap: [production readiness design](superpowers/specs/2026-06-23-production-readiness-design.md)

---

## 1. Purpose and scope

A multi-user URL shortener for a small SaaS running on one VPS with Docker
Compose. Users create short links, anyone can follow them, and owners see how
many times each link was clicked.

**Out of scope for now:** custom short domains, Kubernetes,
WAF, captcha, payments.

---

## 2. Functional requirements

### 2.1 Accounts

| ID | Requirement | Status |
|---|---|---|
| FR-A1 | A user registers with email + password and receives an access and a refresh token | ✅ |
| FR-A2 | Email is unique and case-insensitive (`A@x.com` = `a@x.com`) | ✅ normalized in service; DB unique key on `email` |
| FR-A3 | Password is 8–72 bytes | ✅ |
| FR-A4 | Login returns a new token pair | ✅ |
| FR-A5 | After 5 failed logins an email is locked for 15 minutes, even for the correct password | ✅ |
| FR-A6 | Responses and timing do not reveal whether an email is registered (login) | ✅ |
| FR-A7 | A refresh token can be used exactly once, even under concurrent requests | ✅ Redis `GETDEL` |
| FR-A8 | Logout revokes the current refresh token | ❌ no endpoint |
| FR-A9 | User can reset a forgotten password | ✅ emailed single-use link, 30 min; ends all sessions |
| FR-A10 | Email ownership is verified before links can be created | ❌ |
| FR-A11 | Revoke all sessions of a user (password change, compromise) | ⚠️ done on password reset (`urt:<userID>` index); no endpoint of its own; tokens issued before the index existed are not covered |

### 2.2 Links

| ID | Requirement | Status |
|---|---|---|
| FR-L1 | Authenticated user shortens an `http`/`https` URL and gets a unique short code | ✅ |
| FR-L2 | Optional `expires_at` (RFC 3339), must be in the future; any year (no 2038 limit) | ✅ |
| FR-L3 | URLs that point to private, loopback, link-local, CGNAT or unresolvable hosts are rejected | ✅ checked at creation only (see NFR-S6) |
| FR-L4 | Anyone opening `/{code}` gets a `302` to the original URL | ✅ |
| FR-L5 | Expired or deleted links return `404` | ✅ |
| FR-L6 | Owner lists their links, 20 per page, cursor-paginated | ✅ oldest first (ascending id) |
| FR-L7 | Owner deletes a link; it stops redirecting immediately | ✅ cache invalidated |
| FR-L8 | Only the owner can list or delete their links | ✅ |
| FR-L9 | Daily quota per user (`users.quota_daily`) is enforced on creation | ✅ per UTC day, `429` + `Retry-After`; deleted links do not count; concurrent creates can overshoot slightly |
| FR-L10 | Abuse handling: block known phishing/malware targets, report and disable links | ⚠️ admins can disable links and users (410); no automatic URL check, no user reports |
| FR-L11 | Optional custom alias (3–16 of `A-Za-z0-9_-`, case-sensitive; `api`, `docs`, `healthz`, `metrics` reserved); taken → `409`. `GET /api/links/availability?alias=` checks it while typing (60/min per IP) | ✅ |

### 2.3 Analytics

| ID | Requirement | Status |
|---|---|---|
| FR-S1 | Every successful redirect is counted on the link (`clicks`) | ⚠️ best effort: lost if RabbitMQ is down or a batch is dead-lettered |
| FR-S2 | Per-day click history (`analytics_daily`) | ✅ written by the worker with each batch (UTC day); no history before it shipped |
| FR-S3 | `GET /api/links/{code}/stats` and `/stats/summary` | ⚠️ admins get service-wide stats (`/api/admin/stats`); no per-link stats for owners yet |

### 2.4 Administration

| ID | Requirement | Status |
|---|---|---|
| FR-ADM1 | Only enabled users with role `admin` can call `/api/admin/*`; checked against the database on every request | ✅ `RequireAdmin` |
| FR-ADM2 | The first admin is granted outside HTTP | ✅ `server admin grant <email>` |
| FR-ADM3 | Admins list and search users and links, newest first, paged | ✅ `LIKE` search scans the table (fine at current size) |
| FR-ADM4 | Admins change a user's daily quota | ✅ |
| FR-ADM5 | Disabling a user blocks login, ends sessions, refuses link creation and stops all their links | ✅ the access token keeps working up to 15 min for listing and deleting their own links |
| FR-ADM6 | Admins disable, enable and delete any link; a disabled link answers 410 | ✅ |
| FR-ADM7 | Admins read the audit log | ✅ |
| FR-ADM8 | Dashboard numbers (totals, per-day series) | ✅ `GET /api/admin/stats`, up to 90 days |
| FR-ADM9 | Admin web UI | ❌ planned (plan 3, `shorten-link-gui`) |

### 2.5 Operations endpoints

| ID | Requirement | Status |
|---|---|---|
| FR-O1 | `GET /healthz` reports MySQL, Redis, RabbitMQ status; 503 if any is down | ✅ |
| FR-O2 | Separate liveness (process up) and readiness (dependencies up) probes | ❌ one endpoint does both |
| FR-O3 | `GET /metrics` in Prometheus format, internal only | ❌ nginx already blocks the path; app does not serve it |
| FR-O4 | API documented in OpenAPI and browsable at `/docs` | ✅ |

---

## 3. Non-functional requirements

### 3.1 Performance and capacity

| ID | Requirement | Status |
|---|---|---|
| NFR-P1 | Redirect served from cache without a database query | ✅ |
| NFR-P2 | Redirect latency does not depend on RabbitMQ (publish is async, 2s cap) | ✅ |
| NFR-P3 | Click writes are batched (≤ 500 events or 2s per transaction) | ✅ |
| NFR-P4 | Every request finishes or fails within 10s | ✅ `TimeoutHandler` |
| NFR-P5 | Load-tested target (e.g. 1k redirects/s on the target VPS) | ❌ no load test or target defined |

### 3.2 Security

| ID | Requirement | Status |
|---|---|---|
| NFR-S1 | TLS only; HTTP redirects to HTTPS; HSTS | ✅ nginx + app header |
| NFR-S2 | Security headers (CSP, X-Frame-Options, nosniff, Referrer-Policy) | ✅ |
| NFR-S3 | CORS allow-list from config, deny by default | ✅ |
| NFR-S4 | Rate limits per client IP on redirect, create and auth endpoints; client cannot spoof its IP | ✅ trusts `X-Real-IP` only from private/loopback peers |
| NFR-S5 | Request body ≤ 1 MB (413 otherwise) | ✅ |
| NFR-S6 | SSRF/abuse guard cannot be bypassed by DNS changes after creation | ⚠️ DNS checked once at creation; the server never fetches URLs, so impact is limited to abuse |
| NFR-S7 | Slow-client protection (header/read/write/idle timeouts) | ✅ |
| NFR-S8 | Secrets come from env/volumes, never committed | ✅ `.env`, `keys/` git-ignored |
| NFR-S9 | JWT signing key can be rotated without logging everyone out (`kid`) | ❌ |
| NFR-S10 | Dependencies and Go toolchain free of known vulnerabilities | ✅ toolchain 1.26.9, 0 reachable findings; enforced in CI |
| NFR-S11 | Redis and RabbitMQ require authentication in production | ✅ prod compose refuses to start without `REDIS_PASSWORD` / `RABBITMQ_*` |
| NFR-S12 | Audit trail for security-relevant actions | ⚠️ every admin action is in `audit_log` (same transaction); logins, password resets and user actions are not |

### 3.3 Reliability

| ID | Requirement | Status |
|---|---|---|
| NFR-R1 | Graceful shutdown of server (30s drain) and worker (flush batch) | ✅ |
| NFR-R2 | Worker exits non-zero when a consumer dies, so it is restarted | ✅ |
| NFR-R3 | A failing click batch cannot loop forever | ✅ retried once, then DLQ |
| NFR-R4 | Redis outage does not break redirects | ✅ rate limiter fails open |
| NFR-R5 | Publisher recovers from a dropped RabbitMQ connection | ✅ background reconnect; server also starts without RabbitMQ |
| NFR-R6 | Dead-lettered clicks can be replayed | ❌ DLQ consumer logs and acks |
| NFR-R7 | Lockout counter can never become permanent | ⚠️ `INCR` and `EXPIRE` are separate calls; crash between them leaves a key without TTL |
| NFR-R8 | Concurrent workers cannot deadlock on click updates | ✅ codes updated in sorted order |
| NFR-R9 | Panics are recovered and returned as JSON 500 with request ID | ✅ `Recover` middleware; ID in `X-Request-ID` header and log line |

### 3.4 Data

| ID | Requirement | Status |
|---|---|---|
| NFR-D1 | Schema changes through versioned migrations | ✅ `deploy/migrations` + `make migrate` |
| NFR-D2 | Migrations run automatically on deploy | ✅ `migrate` service runs before server/worker start |
| NFR-D3 | MySQL backups with tested restore | ❌ |
| NFR-D4 | Redis persistence (AOF) so a restart does not log out every user | ✅ prod compose |
| NFR-D5 | Foreign key / cleanup of links when a user is deleted | ❌ no FK, no user deletion |

### 3.5 Observability

| ID | Requirement | Status |
|---|---|---|
| NFR-OB1 | Structured JSON logs with request ID | ✅ `AccessLog` writes one line per request; handlers/services do not log with the request logger yet |
| NFR-OB2 | Metrics: RPS, latency, error rate, cache hit rate, publish errors | ❌ |
| NFR-OB3 | Dashboards and alerts (error rate, DLQ depth, 5xx, dependency down) | ❌ |

### 3.6 Delivery

| ID | Requirement | Status |
|---|---|---|
| NFR-DL1 | Container images for server and worker | ✅ multi-stage `Dockerfile`, distroless nonroot (~11 MB) |
| NFR-DL2 | Production compose with only nginx exposed | ✅ `docker-compose.prod.yml`, smoke-tested end to end |
| NFR-DL3 | CI runs tests, lint, `govulncheck` on every push | ✅ `.github/workflows/ci.yml` (not yet run on GitHub) |
| NFR-DL4 | Deploy and rollback procedure documented | ⚠️ [deploy.md](deploy.md); rollback is manual, no image registry |
| NFR-DL5 | Each server instance gets a unique `NODE_ID` | ⚠️ configurable now; nothing enforces uniqueness |

---

## 4. Limits and constants

| Name | Value | Where |
|---|---|---|
| Access token TTL | 15 min | `service/auth_service.go` |
| Refresh token TTL | 7 days | `service/auth_service.go` |
| bcrypt cost | 12 | `service/auth_service.go` |
| Lockout | 5 failures / 15 min | `service/auth_service.go` |
| Link cache TTL | 24h (capped at expiry) | `service/link_service.go` |
| DNS lookup / publish timeout | 2s each | `service/link_service.go` |
| Redirect rate limit | 60 / min / IP | `cmd/server/main.go` |
| Create rate limit | 10 / min / IP | `cmd/server/main.go` |
| Login / refresh rate limit | 10 / min, 30 / min per IP | `cmd/server/main.go` |
| Register rate limit | 5 / hour / IP | `cmd/server/main.go` |
| nginx rate limit | 100 r/s / IP, burst 200 | `deploy/nginx/nginx.conf` |
| Request timeout | 10s | `cmd/server/main.go` |
| Max body | 1 MB app, 2 MB nginx | `middleware/bodysize.go` |
| Click batch | 500 events or 2s | `messaging/consumer.go` |
| Page size | 20 | `handler/link.go` |

---

## 5. Path to production (prioritized)

Ordered by risk if you launch without it.

**Must have before public launch**

1. ~~Dockerfile + prod compose, Redis/RabbitMQ auth~~ ✅ done
2. ~~Go toolchain + dependency upgrades, `govulncheck`~~ ✅ done
3. ~~CI pipeline~~ ✅ done (verify on first GitHub run)
4. MySQL backups (NFR-D3). Redis AOF is done.
5. Abuse handling: Safe Browsing check on create (FR-L10). Admins can already disable links and users. Public shorteners get abused within days.
6. ~~Enforce `quota_daily` (FR-L9).~~ ✅ done
7. ~~Recover middleware + per-request access log (NFR-R9, OB1).~~ ✅ done
8. Update `openapi.yaml` (FR-O4).

**Should have soon after**

9. Metrics + dashboards + alerts on 5xx rate, DLQ depth, dependency health (FR-O3, NFR-OB2, OB3).
10. Split `/livez` and `/readyz` (FR-O2).
11. Logout, email verification, a revoke-all-sessions endpoint (FR-A8, A10, A11).
12. Atomic lockout increment (Lua or `SET NX EX` + `INCR`) (NFR-R7).

**Later**

13. Push images to a registry from CI and deploy by tag (NFR-DL4).
14. ~~Per-day analytics~~ ✅ done; per-link stats API for owners (FR-S3).
15. JWT `kid` + key rotation (NFR-S9).
16. Load test and capacity target (NFR-P5).
17. Audit log (NFR-S12).
