# Observability (metrics, logs, dashboards, alerts) — design

**Date:** 2026-11-20
**Status:** approved 2026-11-20
**Covers:** FR-O3, NFR-OB2, NFR-OB3 in `docs/spec.md` (roadmap item 9)
**Supersedes:** "Phase 3 — Observability" in `2026-06-23-production-readiness-design.md`

## 1. Intent

The owner wants to see how a real product is observed in production: what
metrics and logs look like, how dashboards and alerts are built, and how they
are used. It is a learning goal, but the result must be production-grade, not
a demo: the same setup runs locally first and later on the VPS with only
configuration changes.

Success: one command brings up the app plus the observability stack locally;
a load script produces traffic; Grafana shows live RED metrics, cache hit rate,
queue/DLQ depth, dependency and container health; logs of any request can be
found by `request_id`; alerts fire (visible in the UI) when things break,
e.g. stopping MySQL or RabbitMQ.

## 2. Goals, non-goals, anti-goals

**Goals**
- Prometheus metrics from `server` and `worker` (`prometheus/client_golang`).
- Container logs collected by Grafana Alloy into Loki, parsed from zap JSON.
- Grafana with provisioned (file-based) datasources and dashboards.
- Prometheus alert rules + Alertmanager.
- Exporters: MySQL, Redis, RabbitMQ (built-in `rabbitmq_prometheus` plugin), cAdvisor.
- k6 load script.
- A local setup that runs `server` and `worker` as containers, so the log path
  is identical to the VPS.
- `request_id` carried from server through RabbitMQ to worker logs (§7).

Built on branch `feat/observability`, from `master` (notifications already
merged).

**Non-goals (this round)**
- Distributed tracing (Tempo / OpenTelemetry). Next round, separately.
- VPS deployment of the stack: no Grafana-behind-nginx, TLS, or external
  alert receiver now. The design must not block them (see §9).
- Split `/livez` / `/readyz` (FR-O2).
- Business analytics for users (already in `analytics_daily`).

**Anti-goals (would be a failure even if it works)**
- Unbounded label cardinality: short codes, user IDs, request IDs, raw paths
  or emails as Prometheus labels or Loki labels.
- `/metrics` reachable from the internet.
- Observability slowing or breaking the redirect hot path. Metrics are
  in-memory counters; the app must not depend on Prometheus/Loki being up.
- Hand-clicked dashboards that exist only inside a Grafana volume.
- Changing the normal dev loop: `make run` / `go run` must keep working
  without the stack.

## 3. Constraints

- Single VPS later: total extra RAM should stay around 1–1.5 GB. Keep default
  retention short (Prometheus 15d, Loki 7d) and configurable.
- Server/worker images are distroless (no shell, uid 65532).
- Local dev today: `docker-compose.yml` runs only MySQL/Redis/RabbitMQ; the
  app runs on the host with `.env` pointing at `localhost`.
- Go 1.26, stdlib `http.ServeMux` with method patterns (`GET /{code}`).
- Promtail is end-of-life (March 2026); use Grafana Alloy.
- Docs-sync rule in `CLAUDE.md` / `CONTRIBUTING.md` applies.

## 4. Approaches considered

| | Approach | Trade-off |
|---|---|---|
| **A (chosen)** | `client_golang` `/metrics` scraped by Prometheus; Alloy → Loki for logs; exporters; Grafana; Alertmanager | Most common self-hosted stack, least code, best docs. Traces later need OTel alongside, which is normal in industry |
| B | OpenTelemetry SDK for metrics + logs, OTLP push to Alloy as collector | Vendor-neutral, traces easy later; more concepts, more verbose Go code, one more hop to debug |
| C | No app code: only exporters + log-derived metrics | Cheap, but no cache hit rate or worker batch metrics; not representative of a real product. Dropped |

Owner picked **A**, because the goal is learning what real products run,
and Prometheus `/metrics` + PromQL is the most visible and common.

Local run mode: owner picked **app in containers** (over `go run` + log file),
so Alloy reads Docker logs exactly as on the VPS.

## 5. Architecture

```
                 ┌──────────── app network ────────────┐
  k6 ──HTTP──▶ server :8080 (API)      worker
               server :9100 /metrics   worker :9100 /metrics
                 │ stdout JSON            │ stdout JSON
                 ▼                        ▼
           Docker json-file logs ──▶ Alloy ──push──▶ Loki ─┐
                                                           ├─▶ Grafana :3000
  Prometheus ──scrape──▶ server, worker, mysqld-exporter,  │
     │                   redis-exporter, rabbitmq :15692,  │
     │                   cadvisor, alloy, loki, itself  ───┘
     └─alerts─▶ Alertmanager :9093
```

### Files

- `docker-compose.observability.yml` — new overlay with `prometheus`,
  `alertmanager`, `loki`, `alloy`, `grafana`, `cadvisor`, `mysqld-exporter`,
  `redis-exporter`; `k6` under profile `tools`. Used together with a compose
  file that runs the app, locally and later on the VPS.
- `docker-compose.yml` — add `server`, `worker` and `migrate` under a compose
  profile `app`, so `docker compose up` (no profile) still runs only the
  dependencies for `go run`. Inside compose the app reaches dependencies by
  service name (override `MYSQL_HOST`, `REDIS_ADDR`, `RABBITMQ_URL`, JWT paths
  like `docker-compose.prod.yml` does).
  Local RabbitMQ gets real credentials (`RABBITMQ_DEFAULT_USER/PASS` from
  `RABBITMQ_USER`/`RABBITMQ_PASSWORD`), because the default `guest` user only
  accepts localhost and the app containers would be refused. `.env.example`'s
  host `RABBITMQ_URL` changes to match; existing developers must update their
  `.env` (called out in README and the change notes).
- `deploy/observability/` — `prometheus/prometheus.yml`,
  `prometheus/rules/*.yml`, `alertmanager/alertmanager.yml`,
  `loki/loki.yml`, `alloy/config.alloy`, `grafana/provisioning/{datasources,dashboards}/`,
  `grafana/dashboards/*.json`, `k6/load.js`.
- `internal/metrics` — new package: metric definitions, the HTTP middleware,
  and the `/metrics` listener helper.
- `Makefile` — `make observe` (build + up app and stack, print URLs),
  `make observe-down`, `make load` (run k6).

### Ports (local, published on 127.0.0.1 only)

Grafana 3000, Prometheus 9090, Alertmanager 9093, server API 8080.
Loki, Alloy, exporters and the app metrics ports stay internal; RabbitMQ
management (15672) as today. Exact numbers are the builder's call if they
clash.

## 6. Application metrics

### Exposure

- New env `METRICS_PORT`, **default `0` (off)**. When set, `server` and
  `worker` each start a second `http.Server` on it serving only `GET /metrics`
  (`promhttp.Handler`) with Go runtime and process collectors. Compose sets
  `METRICS_PORT=9100` for both containers (separate containers, no clash).
  Default off is the designer's call: with `go run` both binaries share one
  `.env` on the host, and a shared non-zero default would make the second one
  fail to bind.
- Separate from the API port, so nginx never routes it; the existing nginx
  `/metrics` block stays as defense in depth. Not published in prod compose.
- Graceful shutdown together with the main server / worker.
- Failure to bind is fatal at startup (misconfiguration should be loud).

### Metric list

Names follow Prometheus conventions, namespace `shortlink_`.

**HTTP (server, middleware)**
- `shortlink_http_requests_total{method, route, status}` counter.
- `shortlink_http_request_duration_seconds{method, route}` histogram, buckets
  0.005 … 5s (e.g. `.005,.01,.025,.05,.1,.25,.5,1,2.5,5`).
- `shortlink_http_requests_in_flight` gauge.
- `route` = the `ServeMux` pattern (e.g. `GET /{code}`, `POST /api/links`);
  unmatched requests use `route="unmatched"`. `status` = numeric code.
  Never the raw path.
- **Known gotcha:** `ServeMux` sets `r.Pattern` on the request it dispatches,
  but middleware such as `JWTAuth`/`RequestID` call `r.WithContext`, so an
  outer middleware holds a different `*http.Request`. The builder must make
  the pattern visible to the metrics middleware (e.g. place the middleware so
  it sees the mux's request, or have a small holder in context that an
  innermost wrapper fills). Covered by a test.
- Placement (designer's call): the metrics middleware is the **outermost**
  wrapper, outside `http.TimeoutHandler`, because a middleware inside it never
  sees the 503 the timeout handler writes. It is also outside `RateLimit`, so
  429s are counted. Since it is outside the mux, it learns the route through a
  holder placed in the request context, which an innermost wrapper around the
  mux fills from `r.Pattern`. When the holder is empty, the route is `unmatched`
  (and for timeouts, whatever was filled before the deadline).

**Redirect / cache (service or repository layer)**
- `shortlink_link_cache_requests_total{result="hit|miss|error"}`.
- `shortlink_redirects_total{outcome="found|not_found|disabled|error"}`.

**Publishing (server and worker `Publisher`)**
- `shortlink_rabbitmq_publish_total{routing_key, result="ok|error"}`.
  `routing_key` is a fixed small set, safe as a label.
- `shortlink_rabbitmq_publisher_connected` gauge (0/1).

**Worker**
- `shortlink_worker_messages_total{queue, result="ack|nack|dlq"}`.
- `shortlink_worker_click_batch_size` histogram (events per flush).
- `shortlink_worker_click_flush_duration_seconds` histogram.
- `shortlink_worker_click_flush_total{result="ok|error"}`.
- `shortlink_worker_expiry_scan_total{result}` and `..._last_success_timestamp_seconds` gauge.

**Business (cheap counters, no IDs)**
- `shortlink_links_created_total`, `shortlink_auth_logins_total{result="ok|fail|locked"}`,
  `shortlink_ratelimit_rejected_total{action}`.

### Code shape

- Metrics registered on a package-level registry in `internal/metrics`
  (not the global default, to keep tests isolated) but exposed through small
  interfaces or plain functions so `service/` does not import Prometheus types
  directly — matching "interfaces live where they are used". Builder chooses
  between interface injection and package functions; tests must be able to
  assert counter increments (`prometheus/testutil`).
- Metric updates must never block or return errors to callers.

## 7. Logs

- Apps keep writing zap JSON to stdout. No logging library change.
- Alloy: `discovery.docker` + `loki.source.docker` via the read-only Docker
  socket, then `loki.process` with a `stage.json` that extracts `level` and
  `request_id` (the other fields stay in the line and are reached with `| json`).
- **Loki labels (low cardinality only):** `service` (compose service name),
  `container`, `level`, `env` (`local`/`prod`). `request_id`, `path`,
  `user_id`, `status` stay in the log line / structured metadata, queried with
  `| json | request_id="..."`.
- Collect all containers (app, MySQL, Redis, RabbitMQ, nginx later).
- Retention 7d (configurable), filesystem storage in a volume.
- Handlers logging via the request logger remains a known gap (NFR-OB1); not
  in scope.

### `request_id` across server → RabbitMQ → worker (owner's choice: in scope)

Goal: searching one `request_id` in Loki shows the server lines **and** the
worker lines for the events that request caused.

- Today `RequestID` puts the id only inside the request's zap logger, and both
  `publishAsync` helpers (`service/link_service.go` method and
  `service/publish.go` function) publish with `context.Background()`, so the
  id is lost. Change: `RequestID` also stores the id as its own context value
  (e.g. in `internal/ctxkey`); both `publishAsync` helpers copy it into the
  detached publish context; `Publisher.Publish` writes it to the AMQP message
  header `x-request-id` when present. Payload JSON is unchanged.
- Safety (designer's call): `X-Request-ID` comes from the client, so it is
  accepted only if it is 1–64 chars of `[A-Za-z0-9_-]`; otherwise a new id is
  generated. This also bounds what goes into headers and logs.
- Worker: the notification consumer logs `request_id` from the header on each
  processed, retried or dead-lettered message. The click consumer handles
  batches of up to 500, so logging every id on success is too noisy: it logs
  the ids only for messages in a failed batch. The DLQ consumer logs the
  header for every dead-lettered message. Events with no originating request
  (the worker's own `link.expiring` scan) carry no header.
- Tests: header set when the context has an id, absent otherwise; invalid
  client ids replaced; consumers log the id.
- This is propagation of one id, not tracing: no spans, no timing per hop.
  Tracing (OpenTelemetry/Tempo) remains next round, and will replace this
  header with W3C `traceparent` or carry both.

## 8. Dashboards and alerts

### Dashboards (JSON, provisioned, read-only in Grafana)

1. **Service overview** — RPS, error rate (5xx / total), p50/p95/p99 latency
   per route, in-flight requests, redirect outcomes, cache hit rate, publish
   errors, publisher connected.
2. **Worker & queues** — queue depth and DLQ depth (RabbitMQ plugin metrics),
   consume rate, batch size, flush duration/errors, last expiry scan age.
3. **Dependencies & host** — MySQL (up, connections, queries/s, slow queries),
   Redis (up, memory, ops/s, hit ratio), RabbitMQ (up, connections), cAdvisor
   CPU/RAM per container.
4. **Logs** — Loki panels: error-level logs by service, log volume by level,
   a `request_id` text variable to trace one request across services.

Panels link metrics to logs where Grafana supports it (e.g. from a 5xx spike
to Loki error logs for the same service and time range).

### Alert rules (Prometheus, `deploy/observability/prometheus/rules/`)

| Alert | Condition (sketch) | For | Severity |
|---|---|---|---|
| `HighErrorRate` | 5xx / all requests > 5% | 5m | critical |
| `HighLatencyP95` | p95 of `GET /{code}` > 300ms | 10m | warning |
| `TargetDown` | `up == 0` for any scrape target | 2m | critical |
| `DependencyDown` | `mysql_up == 0` or `redis_up == 0` (exporters), or the RabbitMQ scrape target is down | 2m | critical |
| `DeadLettered` | `sum by (queue) (increase(shortlink_worker_messages_total{job="worker",result="dlq"}[10m])) > 0` | — | warning |
| `DLQNotDraining` | `clicks.dlq` or `notifications.dlq` ready messages > 0 (worker not consuming them) | 5m | warning |
| `ClickQueueBacklog` | `clicks` ready messages > 10000 | 10m | warning |
| `PublisherDisconnected` | `shortlink_rabbitmq_publisher_connected == 0` | 2m | warning |
| `ExpiryScanStale` | last successful scan older than 3 × interval | — | warning |

Thresholds are starting values; documented as such.

Alertmanager locally: one default receiver with no notification target
(alerts visible in the Alertmanager and Grafana UI). Grouping by `alertname`,
`job`: no Prometheus series carries a `service` label (it exists only in Loki),
and `job` is the compose service name.

### Load script

`deploy/observability/k6/load.js`: register/login one user, create ~50 links,
then mostly redirects (including some unknown codes for 404s), with a few
list calls. Configurable VUs/duration through env. It must respect or
deliberately exercise the per-IP rate limits (60 redirects/min per IP): the
builder picks one and documents it, e.g. a low-RPS default plus a "burst"
scenario that shows 429s on the dashboard.

## 9. Path to the VPS (not built now, design must allow it)

- `docker-compose.observability.yml` combined with `docker-compose.prod.yml`.
- Grafana behind nginx on a subdomain or path, TLS, admin password from
  `.env`, anonymous access off. Prometheus/Alertmanager/Loki never published.
- Alertmanager receiver (email via existing SMTP, or Telegram) from `.env`.
- Retention and resource limits via `.env`.
- Config files must not hard-code `local` assumptions beyond `env` label and
  published ports, which live in the compose overlay / `.env`.

## 10. Testing

- Unit: metrics middleware records `route` as the pattern (not raw path),
  counts 404/429/500/503, `unmatched` for no route; cache hit/miss counters;
  publish ok/error counters; worker ack/nack/flush counters.
- `/metrics` listener: starts on `METRICS_PORT`, serves text format, disabled
  at `0`, shuts down cleanly.
- Config validation: `promtool check config` / `check rules`,
  `amtool check-config`, `alloy fmt`/validate; optional CI step.
- Manual acceptance (documented in `docs/observability.md`): `make observe`,
  `make load`, check each dashboard has data; stop `mysql` and see
  `DependencyDown` fire and recover; find a request's logs by `request_id`.

## 11. Docs to update (same change)

New `docs/observability.md` (how to run, what each metric/dashboard/alert
means, how to query logs). New `docs/modules/metrics.md`, listed in
`docs/modules/README.md` and `docs/architect.md` §5. Update `docs/architect.md`
big picture, `docs/modules/server.md`, `worker.md`, `messaging.md`
(the `x-request-id` header), `service.md`, `middleware.md` (request id
validation), `config.md`; `.env.example` (`METRICS_PORT`, Grafana admin
password, retention); `docs/spec.md` statuses for FR-O3, NFR-OB2, NFR-OB3;
`README.md` run steps; `docs/deploy.md` a short "not yet on VPS" note.
`api/openapi.yaml` unchanged (metrics are not on the API port).

## 12. Left to the builder

- Exact image versions (pin, no `latest`), ports if they clash.
- Exact middleware position, within the §6 requirements.
- Interface injection vs package-level functions for metrics in `service/`.
- Dashboard layout details and whether to start from community dashboards
  (e.g. mysqld/redis exporter dashboards) and trim them.
- Exact Loki/Prometheus storage settings within the retention targets.
- k6 scenario details.
