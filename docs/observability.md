# Observability — metrics, logs, dashboards, alerts

This page is the runbook for the local observability stack: how to start it,
what every metric, dashboard and alert means, how to search logs, and what is
still missing. The design and the reasons behind it are in
[superpowers/specs/2026-11-20-observability-design.md](superpowers/specs/2026-11-20-observability-design.md).
The metric definitions in code are described in [modules/metrics.md](modules/metrics.md).

The stack runs **locally only** for now. Nothing here is deployed to the VPS
yet (design §9 lists what that needs).

## 1. What runs

```
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

- `server` and `worker` run as containers (compose profile `app` in
  `docker-compose.yml`). Each serves Prometheus metrics on port 9100 inside the
  compose network (`METRICS_PORT=9100` is set by compose). With `go run` on the
  host, `METRICS_PORT` defaults to `0` and no metrics listener starts.
- `docker-compose.observability.yml` adds Prometheus, Alertmanager, Loki,
  Alloy, Grafana, cAdvisor, mysqld-exporter and redis-exporter, plus `k6` under
  the `tools` profile.
- All config lives in `deploy/observability/` (Prometheus config and alert
  rules, Alertmanager, Loki, Alloy, Grafana provisioning and dashboard JSON,
  the k6 script, RabbitMQ plugin config).
- RabbitMQ metrics come from its built-in `rabbitmq_prometheus` plugin on
  port 15692 (enabled in `deploy/observability/rabbitmq/enabled_plugins`).

The app never depends on the stack: metrics are in-memory counters that
Prometheus pulls, and logs go to stdout. If Prometheus, Loki or Grafana is
down, requests are not affected; only the data for that period is missing.

## 2. Start and stop

Needs `make keys` once and port 8080 free (stop a host `make run` first).

```bash
make observe-check   # validate Prometheus config + rules, Alertmanager config, Alloy formatting
make observe         # build and start app + dependencies + stack, wait until healthy, print URLs
make load            # 5 min of mixed traffic through k6 (needs make observe running)
make observe-down    # stop everything; all volumes are kept
```

`make load` options (environment variables):

| Variable | Default | Meaning |
|---|---|---|
| `RATE` | `50` | Target requests per second in the `steady` scenario |
| `DURATION` | `5m` | How long to hold `RATE` (after a 10s ramp-up) |
| `SCENARIO` | `steady` | `steady`, or `burst`: one fixed IP at 5 req/s for 2 min, to trigger 429s |

The `steady` mix is about 90% redirects (5% of them unknown codes, so 404),
8% `GET /api/links` and 2% `POST /api/links`, each with a random `X-Real-IP`
so per-IP limits act like many users. k6 exits non-zero if fewer than 99% of
checks pass (any 5xx or connection error fails one), if redirect p95 is
300 ms or more, or if the steady scenario drops iterations because k6 cannot
keep up with `RATE`.

Expected 429s: the throwaway k6 user has `quota_daily = 100` (the default
set at registration in `internal/service/auth_service.go`), so on long runs
`POST /api/links` starts returning 429 once the quota is used. That is
expected and does not fail the run.

### Fresh observability state

To start with empty metrics, logs and Grafana state, remove only the three
stack volumes (names are prefixed with the compose project name, usually the
directory name; check with `docker volume ls`):

```bash
make observe-down
docker volume rm shorten-link_prometheus_data shorten-link_loki_data shorten-link_grafana_data
make observe
```

Do **not** use `docker compose ... down -v` for this: `-v` also deletes
`mysql_data`, your local database.

### URLs and login

All ports bind `127.0.0.1` only.

| What | URL | Login |
|---|---|---|
| Grafana | <http://localhost:3000> | `admin` / `GRAFANA_ADMIN_PASSWORD` from `.env` (default `admin`). Anonymous access and sign-up are off |
| Prometheus | <http://localhost:9090> | none (targets: Status → Targets; rules: Alerts) |
| Alertmanager | <http://localhost:9093> | none |
| RabbitMQ management | <http://localhost:15672> | `RABBITMQ_USER` / `RABBITMQ_PASSWORD` from `.env` (`guest` / `guest` if unset) |
| Server API | <http://localhost:8080> | |

Loki, Alloy, cAdvisor, the exporters and the app metrics ports are not
published; reach them through Grafana or Prometheus.

### Settings

| Variable (`.env`) | Default | Used by |
|---|---|---|
| `GRAFANA_ADMIN_PASSWORD` | `admin` | Grafana admin password |
| `PROMETHEUS_RETENTION` | `15d` | Prometheus `--storage.tsdb.retention.time` |
| `LOKI_RETENTION` | `168h` | Loki `retention_period` (compactor deletes older logs) |
| `MYSQL_ROOT_PASSWORD` | none | mysqld-exporter connects as root with it; must be set |
| `REDIS_PASSWORD` | empty | redis-exporter |

Prometheus scrapes every 15s and evaluates rules every 15s.

## 3. Metrics

### 3.1 Application metrics (`shortlink_*`)

Both `server` and `worker` register the same full set of metrics on their own
registry, plus Go runtime (`go_*`) and process (`process_*`) metrics. A metric
a process never updates still appears: plain counters/gauges/histograms show
`0`, labelled ones show no series until first use. So always filter by `job`
(`job="server"` or `job="worker"`) when the process matters; the dashboards do.

| Name | Type | Labels | Updated by | Meaning |
|---|---|---|---|---|
| `shortlink_http_requests_total` | counter | `method`, `route`, `status` | server | Finished HTTP requests. `route` is the `ServeMux` pattern (e.g. `GET /{code}`, `POST /api/links`) or `unmatched`; `status` is the numeric code. Timeouts count as `503` with their real route |
| `shortlink_http_request_duration_seconds` | histogram | `method`, `route` | server | Request duration. Buckets `.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5` s |
| `shortlink_http_requests_in_flight` | gauge | | server | Requests being served right now |
| `shortlink_link_cache_requests_total` | counter | `result`: `hit`, `miss`, `error` | server | Redis cache lookups on redirect |
| `shortlink_redirects_total` | counter | `outcome`: `found`, `not_found`, `disabled`, `error` | server | Result of resolving a short code. Expired links count as `not_found` |
| `shortlink_links_created_total` | counter | | server | Links created |
| `shortlink_auth_logins_total` | counter | `result`: `ok`, `fail`, `locked` | server | Login attempts |
| `shortlink_ratelimit_rejected_total` | counter | `action` | server | Requests rejected by the per-IP rate limiter. `action` is one of `redirect`, `create`, `login`, `register`, `refresh`, `forgot-password`, `reset-password`, `alias-check` |
| `shortlink_rabbitmq_publish_total` | counter | `routing_key`, `result`: `ok`, `error` | server, worker | Event publishes. Routing keys: `link.created`, `link.clicked`, `link.disabled`, `link.enabled`, `user.password_changed` (server), `link.expiring` (worker) |
| `shortlink_rabbitmq_publisher_connected` | gauge | | server, worker | `1` while the publisher has an open RabbitMQ channel, else `0` |
| `shortlink_worker_messages_total` | counter | `queue`: `clicks`, `notifications`; `result`: `ack`, `nack`, `dlq` | worker | Consumed messages. `nack` = requeued for one retry; `dlq` = rejected without requeue (already redelivered, or an invalid click event) |
| `shortlink_worker_click_batch_size` | histogram | | worker | Click events per flush |
| `shortlink_worker_click_flush_duration_seconds` | histogram | | worker | Time to write one click batch to MySQL |
| `shortlink_worker_click_flush_total` | counter | `result`: `ok`, `error` | worker | Click batch flushes |
| `shortlink_worker_expiry_scan_total` | counter | `result`: `ok`, `error` | worker | Runs of the expiring-links scan |
| `shortlink_worker_expiry_scan_last_success_timestamp_seconds` | gauge | | worker | Unix time of the last successful scan. `0` on the server, which never scans |

The DLQ consumers only log dead-lettered messages; they do not update a
metric. Dead-lettering is counted by the consumers that nack
(`result="dlq"` above), and DLQ depth comes from RabbitMQ (below).

### 3.2 Metrics from exporters

The dashboards and alerts use these (each exporter exposes many more):

| Source (Prometheus `job`) | Metrics used |
|---|---|
| `mysqld-exporter` | `mysql_up`, `mysql_global_status_threads_connected`, `mysql_global_status_threads_running`, `mysql_global_status_queries`, `mysql_global_status_slow_queries` |
| `redis-exporter` | `redis_up`, `redis_memory_used_bytes`, `redis_commands_processed_total`, `redis_keyspace_hits_total`, `redis_keyspace_misses_total` |
| `rabbitmq` (`/metrics`) | `rabbitmq_connections`, `rabbitmq_channels`, and `up{job="rabbitmq"}` for "is RabbitMQ up" |
| `rabbitmq-queues` (`/metrics/detailed?family=queue_coarse_metrics`) | `rabbitmq_detailed_queue_messages_ready{queue=...}`: ready messages per queue (`clicks`, `clicks.dlq`, `notifications`, `notifications.dlq`) |
| `cadvisor` | `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes` (by `name`) |
| every job | `up` (1 if the last scrape worked) |

Prometheus also scrapes `loki`, `alloy`, `alertmanager` and itself
(`prometheus`), mostly so `TargetDown` covers them.

### 3.3 Cardinality rule

Every label value must come from a small, fixed set. **Never** use a short
code, user ID, request ID, email or raw URL path as a Prometheus label or a
Loki label. Each distinct value creates a new time series (or log stream), and
an unbounded set makes Prometheus and Loki slow and memory-hungry. That is why
`route` is the mux pattern (`GET /{code}`), never the path (`/abc123`), and
why `request_id` is structured metadata in Loki, not a label. Put per-request
detail in the log line instead.

## 4. Logs

Apps write zap JSON to stdout. Alloy (`deploy/observability/alloy/config.alloy`)
reads every container's logs through the Docker socket, parses JSON lines and
pushes them to Loki.

- **Loki labels** (the only ones): `service` (compose service name, e.g.
  `server`, `worker`, `mysql`), `container`, `level` (zap level: `debug`,
  `info`, `warn`, `error`; empty for non-JSON lines such as MySQL and
  RabbitMQ), `env` (`local`).
- **Structured metadata:** `request_id`. Filter on it with
  `| request_id="..."` (no `| json` needed).
- Everything else (`path`, `status`, `method`, `user_id`, `duration`, ...)
  stays in the line; use `| json` to filter on it.
- Loki's automatic `service_name` label and `detected_level` metadata are
  turned off in `loki.yml`, so the label set stays the four above.

The server's access log has one line per request with `request_id`. The id
comes from the client's `X-Request-ID` header if it matches
`^[A-Za-z0-9_-]{1,64}$`, otherwise a new one is generated; it is returned in
the `X-Request-ID` response header. It is also carried to the worker in the
AMQP header `x-request-id`. The notification consumer logs it on every
message; the click consumer logs it only for messages in a failed batch; the
DLQ consumer logs it for every dead-lettered message. `link.expiring` events
come from the worker's own scan and have no request id.

### LogQL examples (Grafana → Explore → Loki)

```logql
# All error lines from the app, one panel per service
{service=~"server|worker", level=~"error|dpanic|panic|fatal"}

# Error count per service over time
sum by (service) (count_over_time({level=~"error|dpanic|panic|fatal"}[5m]))

# One request across server and worker (id from the X-Request-ID response header)
{service=~".+"} | request_id="7f3c2a9e-..."

# Server requests that returned 5xx
{service="server"} | json | status >= 500

# Slow access-log lines outside /api (duration is in seconds: zap production encoding)
{service="server"} | json | path !~ "/api/.*" | duration > 0.2

# Dead-lettered messages (the DLQ consumers log them at level error)
{service="worker"} |= "dead-letter message received"
```

To trace a request: send it with your own id, then search it.

```bash
curl -s -o /dev/null -D - -H 'X-Request-ID: trace-me-1' http://localhost:8080/healthz | grep -i x-request-id
# then in Grafana: {service=~".+"} | request_id="trace-me-1"
```

## 5. Dashboards

Provisioned from `deploy/observability/grafana/dashboards/*.json` into the
Grafana folder **shorten-link**. They are read-only in the UI
(`allowUiUpdates: false`): to change one, edit the JSON (or export an edited
copy from Grafana) and commit it. Datasources (Prometheus, Loki, Alertmanager)
are provisioned with fixed UIDs that the dashboards reference.

| Dashboard (uid) | Question it answers | Panels |
|---|---|---|
| **Service overview** (`sl-overview`) | Is the server healthy for users right now? | Requests per second by route; error rate (5xx / total); p50, p95, p99 latency per route; in-flight requests; redirect outcomes; cache hit rate; RabbitMQ publishes by routing key and result; publisher connected; links created / logins / rate-limited. A dashboard link "Error logs (server)" opens the Logs dashboard for the same time range |
| **Worker and queues** (`sl-worker`) | Is the worker keeping up, and is anything stuck or dead-lettered? | Queue depth (ready messages); dead-letter queue depth; consume rate by queue and result; click batch size; click flush duration; click flushes by result; expiry scans by result; last successful expiry scan age |
| **Dependencies and host** (`sl-deps`) | Are MySQL, Redis, RabbitMQ and the containers healthy? | MySQL up, connections, queries/s, slow queries/s; Redis up, memory used, commands/s, keyspace hit ratio; RabbitMQ scrape up, connections and channels; CPU and memory per container (cAdvisor) |
| **Logs** (`sl-logs`) | What went wrong, and what did one request do? | Variables `service` (from Loki labels) and `request_id` (text box). Log volume by level; error logs per service; error log lines; "Request trace" log lines matching `request_id` |

The server-side panels filter `job="server"` and the worker panels
`job="worker"`, because both processes export every `shortlink_*` metric (see
§3.1).

The "Request trace" panel runs
`{service=~".+"} | request_id != "" | request_id="$request_id"`. With the box
empty it matches nothing (the panel is empty), and it does not depend on the
`env` label. Error panels match `level=~"error|dpanic|panic|fatal"`, so
start-up failures logged at fatal level show up too.

## 6. Alerts

Rules: `deploy/observability/prometheus/rules/shortlink.yml`. Thresholds are
starting values; tune them once there is real traffic. Prometheus sends firing
alerts to Alertmanager, which groups them by `alertname` and `job`. Locally
the only receiver (`default`) has **no notification target**: alerts are
visible in the Alertmanager UI (<http://localhost:9093>), in Prometheus
(Alerts page) and in Grafana (Alerting, via the Alertmanager datasource).

| Alert | Fires when | For | Severity | What to check when it fires |
|---|---|---|---|---|
| `HighErrorRate` | More than 5% of server requests returned 5xx over 5m | 5m | critical | Service overview → error rate and requests by route to find the route; Logs → error logs for `server`; `/healthz` (503 means a dependency is down; see `DependencyDown`) |
| `HighLatencyP95` | p95 of `GET /{code}` above 300ms over 5m | 10m | warning | Cache hit rate (a drop means MySQL is serving redirects); MySQL queries/slow queries; container CPU in Dependencies and host |
| `TargetDown` | Any scrape target has `up == 0` | 2m | critical | Prometheus → Status → Targets shows which job and the scrape error; `docker compose ps` for that container; its logs |
| `DependencyDown` | `mysql_up == 0`, `redis_up == 0`, or `up{job="rabbitmq"} == 0` | 2m | critical | `docker compose ps mysql redis rabbitmq` and their logs. Expected impact is in [architect.md §9](architect.md#9-failure-behaviour). Note `mysql_up`/`redis_up` come from the exporters, so an exporter that cannot log in also reports 0 |
| `DeadLettered` | The worker dead-lettered at least one message in the last 10m (`shortlink_worker_messages_total{result="dlq"}` increased, per queue) | — | warning | Worker logs for `dead-letter message received` (they include `request_id` when there was one); Worker and queues → consume rate by queue and result. Usual causes: MySQL down while the worker consumed, or an invalid event |
| `DLQNotDraining` | `clicks.dlq` or `notifications.dlq` has ready messages | 5m | warning | Is the worker running (`TargetDown` for `worker`)? RabbitMQ UI → Queues for the stuck messages; worker logs |
| `ClickQueueBacklog` | `clicks` has more than 10000 ready messages | 10m | warning | Is the worker running (`TargetDown` for `worker`)? Click flush errors and duration in Worker and queues; MySQL health |
| `PublisherDisconnected` | `shortlink_rabbitmq_publisher_connected == 0` on server or worker | 2m | warning | RabbitMQ up? Credentials (`RABBITMQ_USER`/`RABBITMQ_PASSWORD`)? Server logs for reconnect errors. While it fires, click and notification events are dropped (redirects still work) |
| `ExpiryScanStale` | Worker's last successful expiry scan is more than 900s old | — | warning | Worker logs for expiry scan errors; Expiry scans by result panel. 900s is 3 × the default `EXPIRY_SCAN_INTERVAL` (5m): change both together. Scoped to `job="worker"` because the server exports the same gauge as 0 |

Notes:

- **`DLQNotDraining` only fires when the DLQ consumers are not draining.** The
  worker's DLQ consumers log each dead-lettered message and ack it, so while
  the worker runs the DLQ depth goes back to 0 within moments. The alert
  therefore means "messages are piling up in a DLQ and nobody reads them"
  (worker down or stuck). Dead-lettering itself is caught by `DeadLettered`,
  which uses the worker's own counter
  `shortlink_worker_messages_total{result="dlq"}` (Consume rate panel), so it
  fires even though the depth goes back to 0. It has no `for`, and stays active
  while the 10m window still contains the increase.
- **`ExpiryScanStale` has no `for`**, so it fires on the first evaluation over
  the threshold. The worker scans once right at start-up; if that scan fails
  (e.g. MySQL not reachable), the gauge stays `0` and the alert fires at once.

## 7. Known gaps

- **Local only.** No Grafana behind nginx, no TLS, no external alert receiver
  (email/Telegram). Design §9 has the VPS plan.
- **`env="local"` is hard-coded** in `deploy/observability/prometheus/prometheus.yml`
  (`external_labels`; Prometheus does not expand env vars there) and in
  `deploy/observability/alloy/config.alloy` (`stage.static_labels`). Change
  both to `prod` on the VPS.
- **mysqld-exporter connects as MySQL `root`** with `MYSQL_ROOT_PASSWORD`,
  which has no default (the exporter fails to log in if it is unset, and
  `mysql_up` reads 0). On the VPS use a dedicated user with
  `PROCESS, REPLICATION CLIENT, SELECT`.
- **cAdvisor on macOS.** On Docker Desktop / OrbStack it sees the Linux VM, so
  some container names may be partial or missing (on OrbStack in Nov 2026 all
  compose containers showed with full names). The image comes from
  `ghcr.io/google/cadvisor`. It runs `privileged` and mounts `/var/run`
  (which includes the Docker socket); Alloy mounts `docker.sock` read-only.
  `:ro` only stops writes to the socket file, not API calls, so both have full
  Docker API access.
- **No tracing.** `request_id` propagation is one id across server → RabbitMQ
  → worker, not spans or per-hop timing. OpenTelemetry/Tempo is the next round.
- **Handlers and services do not log with the request logger** (NFR-OB1), so
  only the access log line and worker consumer lines carry `request_id`.
- **No metric for DLQ consumption**; use queue depth from RabbitMQ.
- `make observe-check` validates syntax only; it is not run in CI.

## 8. Manual acceptance checklist

Run this after changing the stack (design §10). Start from a fresh state
(§2, "Fresh observability state") if you want to see first-start behaviour.

1. `make observe-check` passes.
2. `make observe` finishes with all containers healthy and prints the URLs.
3. Prometheus → Status → Targets: every job is `UP` (`server`, `worker`,
   `mysqld-exporter`, `redis-exporter`, `cadvisor`, `loki`, `alloy`,
   `alertmanager`, `prometheus`, `rabbitmq`, `rabbitmq-queues`).
4. Prometheus → Alerts: the 9 rules from §6 are loaded and none is firing.
5. `make load` exits 0.
6. Grafana (login `admin`): the folder `shorten-link` has the four dashboards,
   and every panel of Service overview, Worker and queues, and Dependencies and
   host shows data for the load period. Some series are legitimately flat or
   empty in a normal `steady` run: error rate and slow queries stay 0,
   dead-letter queue depth stays 0, in-flight requests are usually 0 at
   scrape time, and "rate-limited" has no series until a limit is hit (run
   `SCENARIO=burst make load` to see it).
7. Logs dashboard: log volume by level shows lines (the error panels are
   empty unless something failed).
8. Stop MySQL: `docker compose stop mysql`. After the 2m `for` (about
   2.5 minutes in total) `DependencyDown` fires and shows in Alertmanager
   (`mysqld-exporter` stays up and reports `mysql_up 0`). Start it again with
   `docker compose start mysql`; the alert resolves at the next evaluations.
9. Trace one request: `curl -H 'X-Request-ID: accept-1' http://localhost:8080/healthz`,
   then in Explore search `{service=~".+"} | request_id="accept-1"` and see the
   server access log line. To see a worker line for the same id, send a
   request that publishes a notification event (for example
   `POST /api/admin/links/{code}/disable` as an admin) with your id: the
   worker logs `notification stored` with the same `request_id`.
10. `make observe-down`.
