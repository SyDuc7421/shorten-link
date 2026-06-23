# Production Readiness Design — shorten-link

**Date:** 2026-06-23
**Target:** Small SaaS — single VPS, Docker Compose, Grafana observability stack, user-facing analytics

---

## Context

The application already has:
- Core link shortening (create, redirect, delete, list) with cursor-based pagination
- JWT RS256 auth with bcrypt, refresh token rotation
- Redis caching + MySQL persistence
- RabbitMQ async click consumer (batch-updates `links.clicks`)
- Sliding-window rate limiting per action (redirect: 60/min, create: 10/min)
- Request ID middleware + structured zap logging
- Graceful shutdown, Swagger/OpenAPI docs, DB migrations
- Full test coverage (unit + integration with testcontainers)

**Known gaps before this plan:**
- `Resolve` does not enforce `expires_at` — expired links redirect anyway
- `/healthz` returns 200 without checking dependencies
- No Dockerfile for the app itself
- No CORS or security-header middleware
- No SSRF protection when shortening URLs
- No account lockout on failed logins
- Worker writes to `links.clicks` (total) but never populates `analytics_daily` (time-series)
- No analytics read API
- No observability stack
- No CI/CD pipeline

---

## Architecture

```
Internet
   │
   ▼
[nginx]  ← TLS termination, DDoS rate limit, bad UA block
   │
   ▼
[server] ← JWT auth, CORS, security headers, per-request timeout, SSRF guard
   │
   ├── MySQL  (links, users, analytics_daily, audit_log)
   ├── Redis  (link cache, token store, rate-limit counters, lockout counters)
   └── RabbitMQ → [worker] → MySQL analytics_daily

[Prometheus] ← scrapes /metrics from server
[Loki + Promtail] ← scrapes container logs
[Grafana] ← queries Prometheus + Loki
```

All services run on one VPS via `docker-compose.prod.yml`.

---

## Phase 1 — App Hardening + Security

### 1.1 Link Expiry Enforcement
`service.LinkService.Resolve` must check `link.ExpiresAt` after fetching from cache/DB.
If `ExpiresAt != nil && time.Now().After(*ExpiresAt)`, return `service.ErrNotFound`.
The handler already maps `ErrNotFound` → 404, so no handler change needed.

### 1.2 Deep Health Check
`GET /healthz` pings all three dependencies and returns JSON:
```json
{ "mysql": "ok", "redis": "ok", "rabbitmq": "ok" }
```
Returns 200 only if all pass; 503 with the failing component name otherwise.
Implementation: `config.Connect` already holds the clients — pass them to a `HealthChecker` struct registered at startup.

### 1.3 Middleware Additions

| Middleware | Behavior |
|---|---|
| **CORS** | Configurable `ALLOWED_ORIGINS` env var (comma-separated). Default: deny all cross-origin. |
| **Security headers** | HSTS, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `Content-Security-Policy: default-src 'none'` |
| **Max body size** | Wrap request body with `http.MaxBytesReader(w, r.Body, 1MB)`. Return 413 on overflow. |
| **Per-request timeout** | `http.TimeoutHandler(root, 10s, "timeout")` wrapping the root mux. |

All middleware added in `internal/middleware/`; wired in `cmd/server/main.go`.

### 1.4 SSRF Protection
In `service.LinkService.Shorten`, after parsing the URL, reject if the resolved host is:
- A private IP range: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `127.0.0.0/8`, `169.254.0.0/16`, `::1`
- A non-HTTP/HTTPS scheme (`file://`, `ftp://`, etc.)

Return a new `service.ErrForbiddenURL` error; handler maps it → 422.

### 1.5 Account Lockout
On each failed `authSvc.Login`, increment a Redis key `lockout:<email>` (TTL: 15 min).
After 5 failures within the window, `Login` returns `service.ErrAccountLocked` → 429.
On successful login, delete the key.
Implemented in `service.AuthService`; no new repository needed (uses existing `tokenCache` Redis client).

### 1.6 Nginx Security Config
```nginx
limit_req_zone  $binary_remote_addr zone=global:10m rate=100r/s;
limit_conn_zone $binary_remote_addr zone=addr:10m;

server {
    limit_req  zone=global burst=200 nodelay;
    limit_conn addr 50;
    # block empty/malicious User-Agents
    if ($http_user_agent ~* "(curl|wget|python|scrapy|nikto|sqlmap)") { return 403; }
    # TLS 1.2+ only
    ssl_protocols TLSv1.2 TLSv1.3;
}
```
TLS via Certbot/Let's Encrypt; auto-renew via cron.

### 1.7 Config
Add `ALLOWED_ORIGINS` to `config.Config`.
Add `.env.example` with all required and optional env vars documented.

---

## Phase 2 — Containerization

### Dockerfile (multi-stage)
Single Dockerfile with two build targets: `server` and `worker`.
Builder stage: `golang:1.25-alpine`, `go build -ldflags="-s -w"`.
Final stage: `gcr.io/distroless/static` — no shell, minimal attack surface.
RSA keys mounted as a Docker secret or volume (not baked into image).

### docker-compose.prod.yml
Services:
- `nginx` — ports 80/443, depends on `server`
- `server` — internal port 8080, env via `.env`
- `worker` — same image as server, different CMD
- `mysql` — volume-mounted data dir
- `redis` — password-protected
- `rabbitmq` — management plugin disabled in prod
- `prometheus`, `grafana`, `loki`, `promtail` — observability stack (Phase 3)

Networks: one internal bridge; only nginx exposed externally.

---

## Phase 3 — Observability

### Prometheus Metrics (`/metrics`)
Use `prometheus/client_golang`. Instrument:
- `http_requests_total{method, path, status}` — Counter
- `http_request_duration_seconds{method, path}` — Histogram (buckets: 5ms–5s)
- `cache_hits_total` / `cache_misses_total` — Counters in `repository.LinkRedis`
- `rabbitmq_publish_errors_total` — Counter in `messaging.Publisher`

`/metrics` is internal-only (nginx blocks external access).

### Loki + Promtail
Promtail scrapes container stdout/stderr via Docker socket.
Existing zap JSON logs are parsed with Promtail's JSON pipeline stage.
Labels: `container`, `level`, `request_id`.

### Grafana Dashboards
Two pre-built dashboards committed as JSON to `deploy/grafana/dashboards/`:
1. **App overview** — RPS, error rate, P50/P95/P99 latency, cache hit rate
2. **Logs explorer** — Loki query panel filtered by `level=error`

---

## Phase 4 — Analytics API

### Worker Fix
`messaging.ClickConsumer.flush` currently calls `repo.BatchUpdateClicks` which only updates `links.clicks`.
Add `repo.BatchUpsertAnalyticsDaily(ctx, counts map[string]int64, date time.Time) error` that does:
```sql
INSERT INTO analytics_daily (link_id, stat_date, clicks)
SELECT id, ?, ? FROM links WHERE short_code = ? AS new_row
ON DUPLICATE KEY UPDATE clicks = analytics_daily.clicks + new_row.clicks
```
Worker calls both in the same flush — transactional correctness not required (eventual consistency acceptable).

### New Endpoints

**`GET /api/links/{code}/stats`**
Query params: `from` (date, default: 30 days ago), `to` (date, default: today).
Response:
```json
{
  "code": "abc123",
  "data": [
    { "date": "2026-06-01", "clicks": 42 },
    { "date": "2026-06-02", "clicks": 17 }
  ]
}
```
Authorization: user must own the link (check `link.UserID == ctxkey.UserID(ctx)`).

**`GET /api/links/{code}/stats/summary`**
Response:
```json
{
  "code": "abc123",
  "total_clicks": 1204,
  "first_click_date": "2026-05-10",
  "last_click_date": "2026-06-22"
}
```

New repository method: `AnalyticsRepository` interface in `service/` layer, implemented in `repository/analytics_mysql.go`.

---

## Phase 5 — CI/CD

### GitHub Actions Workflow (`.github/workflows/deploy.yml`)
Trigger: push to `master`.

Steps:
1. `go test ./...` — must pass
2. `golangci-lint run` — must pass
3. Build and push Docker image to GitHub Container Registry (`ghcr.io`)
4. SSH into VPS: `docker compose -f docker-compose.prod.yml pull && docker compose -f docker-compose.prod.yml up -d`
5. Run migrations via a dedicated `migrate` container (separate Docker image with `golang-migrate` binary) — distroless server image has no shell

Secrets stored in GitHub Actions: `VPS_HOST`, `VPS_USER`, `VPS_SSH_KEY`, `GHCR_TOKEN`.

### Rollback
If health check fails post-deploy, re-deploy previous image tag manually.
(Automated rollback is out of scope for Phase 5.)

---

## Testing Strategy

| Phase | Tests |
|---|---|
| 1 | Unit tests for expiry check, lockout logic, SSRF guard, each new middleware |
| 2 | `docker compose up` smoke test — all services healthy |
| 3 | Metrics endpoint returns valid Prometheus text format |
| 4 | Unit + integration tests for analytics repository and handler |
| 5 | CI green on master before deploy |

---

## Out of Scope

- Kubernetes / Helm (single VPS is sufficient for Small SaaS)
- Automated rollback
- Email verification / password reset
- Custom short domains
- Captcha
- WAF (Web Application Firewall)
