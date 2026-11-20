# Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prometheus metrics in `server` and `worker`, `request_id` carried through RabbitMQ, and a local Docker stack (app + Prometheus, Alertmanager, Loki, Alloy, Grafana, exporters, k6) with provisioned dashboards and alert rules.

**Architecture:** A new `internal/metrics` package owns a private Prometheus registry, every metric, a tiny `/metrics` listener and the HTTP middleware. Services, middleware and messaging call plain functions in that package (no Prometheus types cross package boundaries). A compose overlay `docker-compose.observability.yml` adds the stack on top of `docker-compose.yml`, whose new `app` profile runs `server`, `worker` and `migrate` in containers.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang`, zap, amqp091-go, Docker Compose v2, Prometheus 3.x, Alertmanager 0.28+, Loki 3.x, Grafana Alloy 1.x, Grafana 12.x, mysqld-exporter, redis_exporter, cAdvisor, k6.

**Spec:** `docs/superpowers/specs/2026-11-20-observability-design.md`

## Global Constraints

- Metric namespace `shortlink_`. Never use short code, user ID, request ID, email or raw path as a Prometheus label or a Loki label.
- `route` label = `ServeMux` pattern (e.g. `GET /{code}`); no match → `unmatched`.
- HTTP histogram buckets: `.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5`.
- `METRICS_PORT` default `0` = off; compose sets `9100` for both containers. Bind failure is fatal.
- Metric updates never block, never return errors to callers.
- `X-Request-ID` from client accepted only if it matches `^[A-Za-z0-9_-]{1,64}$`; otherwise generate a new one. AMQP header name `x-request-id`.
- Loki labels only: `service`, `container`, `level`, `env`. `request_id` goes to structured metadata.
- Retention defaults: Prometheus `15d`, Loki `7d` (168h), both overridable from `.env`.
- Local published ports bind `127.0.0.1` only: Grafana 3000, Prometheus 9090, Alertmanager 9093, server 8080, RabbitMQ 5672/15672 (as today), MySQL 3306, Redis 6379.
- Every image pinned to an exact version (no `latest`, no bare major). Pick the newest stable at build time.
- `make run` / `go run` must keep working with `docker compose up -d` (no profile) exactly as before, apart from the RabbitMQ credentials change.
- Docs ship in the same commit as the code they describe (`CLAUDE.md` §5).
- Tests are stdlib `testing` (no testify), black-box `_test` packages where the package already does that. Counter assertions use `prometheus/testutil.ToFloat64` and compare **deltas** (before/after), since metrics are package-level.

## Review Focus

1. Request that times out (10s `TimeoutHandler`) must count as `status="503"` with its real route, not `unmatched` — Task 2 test `TestMiddleware_TimeoutCountedAs503`.
2. Unknown short code on `GET /{code}` must be `route="GET /{code}"`, `status="404"`, not a new series per code — Task 2 test `TestMiddleware_RouteIsPatternNotPath`.
3. Hostile `X-Request-ID` (200 chars, newline, unicode) must not reach logs or AMQP headers — Task 5 test `TestRequestID_RejectsInvalidHeader`.
4. Publish while RabbitMQ is down must still count `result="error"` and not block the redirect — Task 4 test `TestPublish_NotConnectedCountsError`.
5. Running `make run` and `go run ./cmd/worker` together with default `.env` must not fail on a metrics port clash — Task 1 test `TestServe_ZeroPortDisabled` plus `.env.example` leaving `METRICS_PORT` unset/0.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/metrics/metrics.go` (new) | Registry, all metric definitions, small recording functions |
| `internal/metrics/server.go` (new) | `Serve` — the `/metrics` listener |
| `internal/metrics/http.go` (new) | HTTP middleware + route holder |
| `internal/metrics/*_test.go` (new) | Tests for the above |
| `internal/ctxkey/ctxkey.go` | add request-id context value |
| `internal/middleware/requestid.go` | validate header, store id in context |
| `internal/middleware/ratelimit.go` | count 429s |
| `internal/service/link_service.go`, `publish.go`, `auth_service.go`, `expiry_scanner.go` | record outcomes; carry request id into publish context |
| `internal/messaging/publisher.go`, `consumer.go`, `notification_consumer.go` | publish counter, header, worker counters, request-id logging |
| `config/config.go` | `MetricsPort int` |
| `cmd/server/main.go`, `cmd/worker/main.go` | wire listener, middleware, publisher gauge |
| `docker-compose.yml` | healthchecks, RabbitMQ creds, `app` profile |
| `docker-compose.observability.yml` (new) | stack overlay |
| `deploy/observability/**` (new) | Prometheus, rules, Alertmanager, Loki, Alloy, Grafana provisioning + dashboards, k6 |
| `Makefile` | `observe`, `observe-down`, `load`, `observe-check` |
| `docs/observability.md`, `docs/modules/metrics.md` (new) + existing docs | docs |

---

### Task 1: `internal/metrics` core + `/metrics` listener + config

**Files:**
- Create: `internal/metrics/metrics.go`, `internal/metrics/server.go`, `internal/metrics/server_test.go`
- Modify: `config/config.go`, `go.mod`/`go.sum`, `.env.example`, `docs/modules/config.md`
- Create: `docs/modules/metrics.md`; list it in `docs/modules/README.md` and `docs/architect.md` §5

**Interfaces:**
- Produces:
  - `var Registry *prometheus.Registry` — has Go runtime + process collectors registered.
  - Every metric below as an exported package var, registered on `Registry` in `init()`:

    | Var | Type | Name | Labels |
    |---|---|---|---|
    | `HTTPRequests` | CounterVec | `shortlink_http_requests_total` | method, route, status |
    | `HTTPDuration` | HistogramVec | `shortlink_http_request_duration_seconds` | method, route |
    | `HTTPInFlight` | Gauge | `shortlink_http_requests_in_flight` | — |
    | `CacheRequests` | CounterVec | `shortlink_link_cache_requests_total` | result (`hit`/`miss`/`error`) |
    | `Redirects` | CounterVec | `shortlink_redirects_total` | outcome (`found`/`not_found`/`disabled`/`error`) |
    | `LinksCreated` | Counter | `shortlink_links_created_total` | — |
    | `Logins` | CounterVec | `shortlink_auth_logins_total` | result (`ok`/`fail`/`locked`) |
    | `RateLimited` | CounterVec | `shortlink_ratelimit_rejected_total` | action |
    | `Publishes` | CounterVec | `shortlink_rabbitmq_publish_total` | routing_key, result (`ok`/`error`) |
    | `WorkerMessages` | CounterVec | `shortlink_worker_messages_total` | queue, result (`ack`/`nack`/`dlq`) |
    | `ClickBatchSize` | Histogram | `shortlink_worker_click_batch_size` | — buckets `1,5,10,25,50,100,250,500` |
    | `ClickFlushDuration` | Histogram | `shortlink_worker_click_flush_duration_seconds` | — default buckets |
    | `ClickFlushes` | CounterVec | `shortlink_worker_click_flush_total` | result |
    | `ExpiryScans` | CounterVec | `shortlink_worker_expiry_scan_total` | result |
    | `ExpiryScanLastSuccess` | Gauge | `shortlink_worker_expiry_scan_last_success_timestamp_seconds` | — |

  - `func RegisterPublisherConnected(connected func() bool)` — registers GaugeFunc `shortlink_rabbitmq_publisher_connected` (1/0). Called once per process.
  - `func Serve(ctx context.Context, port int, log *zap.Logger) error` — `port == 0` returns `nil` immediately (disabled). Otherwise binds `:port` **synchronously** (`net.Listen`; bind error returned), serves `GET /metrics` with `promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})` in a goroutine, and shuts down (5s) when `ctx` is done.
  - `config.Config.MetricsPort int` — `envconfig:"METRICS_PORT" default:"0"`.

- [ ] **Step 1:** `go get github.com/prometheus/client_golang@<latest stable>`.
- [ ] **Step 2: Write failing tests** in `server_test.go`:
  - `TestServe_ZeroPortDisabled`: `Serve(ctx, 0, zap.NewNop())` returns nil, nothing listens.
  - `TestServe_ExposesMetrics`: pick a free port, `Serve`, `GET /metrics` → 200, body contains `shortlink_http_requests_in_flight` and `go_goroutines`.
  - `TestServe_PortInUseErrors`: occupy a port with `net.Listen`, `Serve` on it returns non-nil error.
  - `TestServe_StopsOnCancel`: cancel ctx, then dialing the port fails within 6s.
- [ ] **Step 3:** Run `go test ./internal/metrics/ -run TestServe -v` → FAIL (undefined).
- [ ] **Step 4:** Implement `metrics.go`, `server.go`, config field. `.env.example`: add `METRICS_PORT=0` under a `# Metrics` heading with a comment "0 = off; compose sets 9100 inside containers".
- [ ] **Step 5:** `go test ./internal/metrics/ ./config/ -v` → PASS; `go vet ./...` clean.
- [ ] **Step 6:** Write `docs/modules/metrics.md` (purpose, metric table, how to add a metric, cardinality rule); update `docs/modules/config.md`.
- [ ] **Step 7: Commit** `feat(metrics): add metrics registry and /metrics listener`.

---

### Task 2: HTTP middleware + server/worker wiring

**Files:**
- Create: `internal/metrics/http.go`, `internal/metrics/http_test.go`
- Modify: `cmd/server/main.go`, `cmd/worker/main.go`, `docs/modules/server.md`, `docs/modules/worker.md`, `docs/modules/middleware.md`, `docs/architect.md` (middleware table: new row 0 "Metrics", outermost)

**Interfaces:**
- Consumes: Task 1 `HTTPRequests`, `HTTPDuration`, `HTTPInFlight`, `Serve`, `RegisterPublisherConnected`, `config.MetricsPort`.
- Produces:
  - `func Middleware(next http.Handler) http.Handler` — outermost wrapper. Puts a `*routeHolder` (unexported, mutex- or atomic-guarded string) in the request context, wraps the `ResponseWriter` to capture status (default 200), calls `next`, then records `HTTPRequests{method, route, status}`, `HTTPDuration{method, route}` and `HTTPInFlight` inc/dec. Empty holder → `route="unmatched"`.
  - `func RecordRoute(mux *http.ServeMux) http.Handler` — innermost wrapper. Per request: `_, pattern := mux.Handler(r)` only to read the pattern, store it in the holder (if non-empty), then `mux.ServeHTTP(w, r)`. Do **not** call the handler returned by `mux.Handler` directly: that skips path-value population and `r.PathValue("code")` would be empty. The holder is filled **before** the handler runs, so a request cut off by `TimeoutHandler` still has its route. Holder writes (inner goroutine under `TimeoutHandler`) and reads (outer) race, so guard it (`atomic.Pointer[string]` or mutex); `go test -race` must pass.
  - `method` label = `r.Method` if one of GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS, else `OTHER`.

Wiring in `cmd/server/main.go`:
- `root := withRateLimits(metrics.RecordRoute(mux), rdb)` — note `withRateLimits` dispatches on path and wraps `mux`; change its parameter so every branch wraps the `RecordRoute`-wrapped handler.
- `chain := metrics.Middleware(http.TimeoutHandler(...existing...))`.
- Before `srv.ListenAndServe`: `metricsCtx, stopMetrics := context.WithCancel(ctx)`; `if err := metrics.Serve(metricsCtx, cfg.MetricsPort, log); err != nil { log.Fatal(...) }`; call `stopMetrics()` during shutdown.
- `metrics.RegisterPublisherConnected(func() bool { return !pub.IsClosed() })`.

Wiring in `cmd/worker/main.go`: `metrics.Serve(ctx, cfg.MetricsPort, log)` (fatal on error) after logger; `RegisterPublisherConnected` for its publisher.

- [ ] **Step 1: Write failing tests** in `http_test.go` (build a real `http.ServeMux` with `GET /{code}` and `POST /api/links`, wrap as `Middleware(RecordRoute(mux))`):
  - `TestMiddleware_RouteIsPatternNotPath`: two requests `GET /abc` and `GET /xyz` returning 404 → delta of `HTTPRequests{GET,"GET /{code}","404"}` == 2; `testutil.CollectAndCount(HTTPRequests)` grows by at most 1.
  - `TestMiddleware_Unmatched`: `DELETE /nope/deep` → `route="unmatched"`, status `404` or `405` as mux returns.
  - `TestMiddleware_TimeoutCountedAs503`: `Middleware(http.TimeoutHandler(RecordRoute(mux), 50*time.Millisecond, "x"))`, handler sleeps 200ms → `{GET,"GET /{code}","503"}` +1.
  - `TestMiddleware_RequestThroughWithContext`: insert a middleware between that does `r.WithContext(context.WithValue(...))` (simulating `JWTAuth`) → route still the pattern.
  - `TestMiddleware_DefaultStatus200`: handler writes body without `WriteHeader` → `status="200"`.
  - `TestMiddleware_InFlightReturnsToZero`: after request, `ToFloat64(HTTPInFlight)` back to its starting value.
  - `TestMiddleware_UnknownMethodIsOther`: method `BREW` → `method="OTHER"`.
  - `TestRecordRoute_PathValueStillWorks`: handler for `GET /{code}` writes `r.PathValue("code")`; `GET /abc` body == `abc`.
- [ ] **Step 2:** `go test -race ./internal/metrics/ -run 'TestMiddleware|TestRecordRoute' -v` → FAIL.
- [ ] **Step 3:** Implement `http.go`; wire both mains.
- [ ] **Step 4:** `go test ./internal/metrics/ ./cmd/... -v` → PASS; `go build ./...`.
- [ ] **Step 5:** Manual: `METRICS_PORT=9100 make run`, `curl -s localhost:8080/doesnotexist; curl -s localhost:9100/metrics | grep shortlink_http_requests_total` shows `route="GET /{code}"`.
- [ ] **Step 6:** Update docs listed above (middleware chain comment in `main.go` too).
- [ ] **Step 7: Commit** `feat(metrics): HTTP RED metrics and /metrics listener in server and worker`.

---

### Task 3: Service-level metrics (cache, redirects, links, logins, rate limit)

**Files:**
- Modify: `internal/service/link_service.go`, `internal/service/auth_service.go`, `internal/middleware/ratelimit.go`
- Test: `internal/service/link_service_test.go`, `internal/service/auth_service_test.go`, `internal/middleware/ratelimit_test.go`
- Docs: `docs/modules/service.md`, `docs/modules/middleware.md`

**Interfaces:**
- Consumes: Task 1 `CacheRequests`, `Redirects`, `LinksCreated`, `Logins`, `RateLimited`.
- Rules:
  - `LinkService.Resolve`: `cache.Get` err nil → `hit` (including the disabled marker); `errors.Is(err, ErrNotFound)` → `miss`; other error → `error`. Final returned error: nil → `found`; `ErrNotFound` → `not_found`; `ErrLinkDisabled` → `disabled`; else `error`. Implement the outcome with one deferred classification over a named return.
  - `LinkService.Shorten`: `LinksCreated.Inc()` only on success.
  - `AuthService.Login`: `ErrAccountLocked` → `locked`; `ErrInvalidCredentials`, `ErrAccountDisabled` → `fail`; success → `ok`; any other error (infra) → not counted.
  - `RateLimit`: `RateLimited.WithLabelValues(action).Inc()` right where it writes 429.

- [ ] **Step 1: Write failing tests** (extend existing test files and their fakes):
  - `TestResolve_CacheHitCountsHitAndFound`, `TestResolve_CacheMissDBHitCountsMissAndFound`, `TestResolve_CacheErrorCountsError`, `TestResolve_UnknownCodeCountsNotFound`, `TestResolve_DisabledCountsDisabled`, `TestShorten_OKIncrementsLinksCreated`, `TestShorten_InvalidURLDoesNotIncrement`.
  - `TestLogin_MetricsByResult`: table over ok / wrong password / locked → `Logins` delta per label == 1.
  - `TestRateLimit_RejectionCounted`: exceed limit for action `"test"` → `RateLimited{"test"}` +1 (uses the existing Redis test setup in `ratelimit_test.go`).
- [ ] **Step 2:** `go test ./internal/service/ ./internal/middleware/ -v` → new tests FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/service/ ./internal/middleware/ -v` → PASS.
- [ ] **Step 5:** Update docs.
- [ ] **Step 6: Commit** `feat(metrics): cache, redirect, link, login and rate-limit counters`.

---

### Task 4: Messaging + worker metrics

**Files:**
- Modify: `internal/messaging/publisher.go`, `internal/messaging/consumer.go`, `internal/messaging/notification_consumer.go`, `internal/service/expiry_scanner.go`
- Test: `internal/messaging/publisher_test.go`, `consumer_test.go`, `notification_consumer_test.go`, `internal/service/expiry_scanner_test.go`
- Docs: `docs/modules/messaging.md`, `docs/modules/worker.md`

**Interfaces:**
- Consumes: Task 1 `Publishes`, `WorkerMessages`, `ClickBatchSize`, `ClickFlushDuration`, `ClickFlushes`, `ExpiryScans`, `ExpiryScanLastSuccess`.
- Rules:
  - `Publisher.Publish`: exactly one `Publishes{routingKey, ok|error}` per call, on every return path (marshal error, not connected, retry failure, success).
  - Click consumer: invalid event → `WorkerMessages{clicks, dlq}`. `flush`: observe `ClickBatchSize(len(pending))` and duration; success → `ClickFlushes{ok}`, `WorkerMessages{clicks, ack}` += len; failure → `ClickFlushes{error}`, then per delivery `nack` if requeued (`!d.Redelivered`) else `dlq`. Empty flush records nothing.
  - Notification consumer: same `ack`/`nack`/`dlq` mapping with `queue="notifications"`, at each place it settles a delivery (including `reject` and the dropped `link.expiring` case → `ack` since it is acked).
  - Use the queue name constants (`QueueClicks`, `QueueNotifications`) as label values.
  - `ExpiryScanner.Run`: after each `ScanOnce`, `ExpiryScans{ok|error}`; on ok set `ExpiryScanLastSuccess` to `time.Now()` Unix seconds.

- [ ] **Step 1: Write failing tests:**
  - `TestPublish_NotConnectedCountsError`: publisher with a dial that always fails → `Publish` returns error quickly and `Publishes{"link.clicked","error"}` +1. (No container needed.)
  - Extend an existing container test: successful publish → `{…,"ok"}` +1.
  - `TestClickFlush_SuccessCountsAckAndBatch`, `TestClickFlush_FailureCountsNackThenDLQ` (one fresh + one redelivered delivery → nack +1, dlq +1), using the existing `fakeAck`.
  - `TestNotificationConsumer_Metrics`: table over ok insert / malformed / failing insert first try / redelivered failure.
  - `TestExpiryScanner_RunRecordsScan`: run with a short interval and a fake repo, cancel after first scan → `ExpiryScans{ok}` ≥1 and `ExpiryScanLastSuccess` > 0; failing repo → `{error}` ≥1.
- [ ] **Step 2:** `go test ./internal/messaging/ ./internal/service/ -v` → new tests FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command → PASS.
- [ ] **Step 5:** Update docs.
- [ ] **Step 6: Commit** `feat(metrics): publish, consumer, click batch and expiry scan metrics`.

---

### Task 5: `request_id` through RabbitMQ

**Files:**
- Modify: `internal/ctxkey/ctxkey.go`, `internal/middleware/requestid.go`, `internal/service/publish.go`, `internal/service/link_service.go` (`publishAsync` method), `internal/messaging/publisher.go`, `consumer.go`, `notification_consumer.go`
- Test: `internal/middleware/requestid_test.go`, `internal/service/*_test.go`, `internal/messaging/*_test.go`
- Docs: `docs/modules/middleware.md`, `service.md`, `messaging.md`, `worker.md`, `docs/architect.md` §2/§3 flow notes

**Interfaces:**
- Produces:
  - `func ctxkey.SetRequestID(ctx context.Context, id string) context.Context`
  - `func ctxkey.RequestID(ctx context.Context) (string, bool)`
  - `const messaging.HeaderRequestID = "x-request-id"`
  - `func messaging.newPublishing(ctx context.Context, body []byte) amqp.Publishing` (unexported, pure) — today's fields plus `Headers: amqp.Table{HeaderRequestID: id}` when the context has an id; `Headers` nil otherwise.
  - `func messaging.requestIDOf(d amqp.Delivery) string` (unexported) — header value if a non-empty string, else `""`.
- Rules:
  - `RequestID` middleware: header valid per Global Constraints → reuse, else generate. Store via `ctxkey.SetRequestID` as well as in the logger.
  - Both `publishAsync` helpers: read id from the request ctx **before** the goroutine; build `pctx` from `context.Background()` + timeout + `ctxkey.SetRequestID` if present.
  - Logging field name everywhere: `request_id` (zap `String`), only when non-empty.
  - Notification consumer: one `Info` "notification stored" per successful message with `routing_key` and `request_id` (if any); its existing warn/error lines gain `request_id`.
  - Click consumer: on flush failure, one `Error` line listing `request_ids` (`zap.Strings`, non-empty only, max 500). Success path logs nothing new.
  - DLQ consumer: existing error line gains `request_id`.

- [ ] **Step 1: Write failing tests:**
  - `TestRequestID_RejectsInvalidHeader`: table — 65 chars, `"a\nb"`, `"ä"`, `"a b"`, `""` → response header is a fresh 32-hex id, not the input. Valid `"abc_DEF-123"` kept.
  - `TestRequestID_StoredInContext`: next handler sees `ctxkey.RequestID` == response header.
  - `TestNewPublishing_HeaderFromContext` / `TestNewPublishing_NoHeaderWithoutID`.
  - `TestPublishAsync_CarriesRequestID`: fake `Publisher` captures ctx; `ctxkey.RequestID(captured)` == "rid-1" (both the function in `publish.go` and the `LinkService` method via `Resolve`).
  - `TestNotificationConsumer_LogsRequestID` with `zaptest/observer`: delivery with header → stored-line has `request_id`.
  - `TestClickFlush_FailureLogsRequestIDs`, `TestDLQConsumer_LogsRequestID` (DLQ: refactor the per-message body into an unexported `handle(d amqp.Delivery)` for testability).
- [ ] **Step 2:** `go test ./internal/... -run 'RequestID|Publishing|PublishAsync' -v` → FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/... -race` → PASS.
- [ ] **Step 5:** Update docs.
- [ ] **Step 6: Commit** `feat(observability): carry request_id from server through RabbitMQ to worker logs`.

---

### Task 6: Local compose runs the app in containers

**Files:**
- Modify: `docker-compose.yml`, `.env.example`, `README.md`, `Makefile`

**Changes:**
- Add healthchecks to `mysql`, `redis`, `rabbitmq` (copy from `docker-compose.prod.yml`; redis one must work with an empty password too).
- `rabbitmq`: `RABBITMQ_DEFAULT_USER: ${RABBITMQ_USER}`, `RABBITMQ_DEFAULT_PASS: ${RABBITMQ_PASSWORD}`; mount `deploy/observability/rabbitmq/enabled_plugins` containing `[rabbitmq_management,rabbitmq_prometheus].` (create the file here).
- Bind every published port to `127.0.0.1`.
- New services under `profiles: ["app"]`: `migrate` (as prod), `server` and `worker` built from `Dockerfile` targets, `env_file: .env`, environment overrides `MYSQL_HOST: mysql`, `REDIS_ADDR: redis:6379`, `RABBITMQ_URL: amqp://${RABBITMQ_USER}:${RABBITMQ_PASSWORD}@rabbitmq:5672/`, `JWT_*` → `/keys/...`, `METRICS_PORT: "9100"`, `PASSWORD_RESET_URL` stays localhost so empty SMTP is allowed; `./keys:/keys:ro`; server publishes `127.0.0.1:8080:8080`; `depends_on` with health + `migrate` completed. Worker `restart: unless-stopped`.
- `.env.example`: `RABBITMQ_URL=amqp://shortlink:changeme_rabbit@localhost:5672/` (matches `RABBITMQ_USER`/`RABBITMQ_PASSWORD`).
- README: "Upgrading: recreate RabbitMQ (`docker compose up -d --force-recreate rabbitmq`) and update `RABBITMQ_URL` in your `.env`"; how to run the app in containers.

- [ ] **Step 1:** `docker compose config -q && docker compose --profile app config -q` → no output.
- [ ] **Step 2:** `docker compose up -d --wait` then `make run` → server starts and `/healthz` reports rabbitmq ok (host dev path still works).
- [ ] **Step 3:** stop `make run`; `docker compose --profile app up -d --build --wait`; `curl -s localhost:8080/healthz` → `{"mysql":"ok","rabbitmq":"ok","redis":"ok"}`; `docker compose exec rabbitmq wget -qO- localhost:15692/metrics | head` (or `curl` from host if published) shows `rabbitmq_` metrics.
- [ ] **Step 4: Commit** `build(compose): run server and worker locally in containers (app profile)`.

---

### Task 7: Observability stack overlay + alert rules

**Files:**
- Create: `docker-compose.observability.yml`, `deploy/observability/prometheus/prometheus.yml`, `deploy/observability/prometheus/rules/shortlink.yml`, `deploy/observability/alertmanager/alertmanager.yml`, `deploy/observability/loki/loki.yml`, `deploy/observability/alloy/config.alloy`, `deploy/observability/grafana/provisioning/datasources/datasources.yml`, `deploy/observability/grafana/provisioning/dashboards/dashboards.yml`
- Modify: `Makefile`, `.env.example`

**Decisions:**
- Services: `prometheus` (`--storage.tsdb.retention.time=${PROMETHEUS_RETENTION:-15d}`, `--web.enable-lifecycle`), `alertmanager`, `loki`, `alloy`, `grafana`, `cadvisor`, `mysqld-exporter`, `redis-exporter`. Named volumes for prometheus, loki, grafana data. `restart: unless-stopped`.
- Scrape jobs (interval 15s), `job` = service name: `server:9100`, `worker:9100`, `mysqld-exporter:9104`, `redis-exporter:9121`, `cadvisor:8080`, `loki:3100`, `alloy:12345`, `alertmanager:9093`, `prometheus:9090`; RabbitMQ two jobs: `rabbitmq` → `rabbitmq:15692/metrics` and `rabbitmq-queues` → `rabbitmq:15692/metrics/detailed` with `params: {family: [queue_coarse_metrics]}` (gives `rabbitmq_detailed_queue_messages_ready{queue=…}`). Global external label `env: ${ENV_NAME}` is not expandable in Prometheus config, so set `env: local` in `external_labels` and note it in docs as the one value to change on VPS.
- mysqld-exporter: connects as `root` with `MYSQL_ROOT_PASSWORD` locally. Document as a known gap: VPS needs a dedicated user with `PROCESS, REPLICATION CLIENT, SELECT`.
- redis-exporter: `REDIS_ADDR=redis://redis:6379`, `REDIS_PASSWORD`.
- cAdvisor: standard read-only mounts; document that on Docker Desktop (macOS) container names may be partial.
- Alloy: mount `/var/run/docker.sock:ro`; `discovery.docker` → `discovery.relabel` (label `service` from `__meta_docker_container_label_com_docker_compose_service`, `container` from container name) → `loki.source.docker` → `loki.process` (`stage.json` extracting `level`, `request_id`; `stage.labels` `level`; `stage.structured_metadata` `request_id`; `stage.static_labels` `env="local"`) → `loki.write` to `http://loki:3100/loki/api/v1/push`.
- Loki: single binary, filesystem storage, TSDB schema v13, `allow_structured_metadata: true`, `retention_period: ${LOKI_RETENTION:-168h}` via `-config.expand-env=true`, compactor retention enabled.
- Grafana: `GF_SECURITY_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD:-admin}`, anonymous off; datasources with fixed UIDs `prometheus` and `loki` (dashboards in Task 8 reference these). Alertmanager datasource too. Dashboard provider points to `/var/lib/grafana/dashboards`, `allowUiUpdates: false`.
- Alertmanager: single receiver `default` with no configs, `group_by: [alertname, job]`.
- Alert rules — exact expressions:

  | Alert | expr | for | severity |
  |---|---|---|---|
  | `HighErrorRate` | `sum(rate(shortlink_http_requests_total{status=~"5.."}[5m])) / sum(rate(shortlink_http_requests_total[5m])) > 0.05` | 5m | critical |
  | `HighLatencyP95` | `histogram_quantile(0.95, sum by (le) (rate(shortlink_http_request_duration_seconds_bucket{route="GET /{code}"}[5m]))) > 0.3` | 10m | warning |
  | `TargetDown` | `up == 0` | 2m | critical |
  | `DependencyDown` | `mysql_up == 0 or redis_up == 0 or up{job="rabbitmq"} == 0` | 2m | critical |
  | `DLQNotEmpty` | `rabbitmq_detailed_queue_messages_ready{queue=~"clicks.dlq\|notifications.dlq"} > 0` | 5m | warning |
  | `ClickQueueBacklog` | `rabbitmq_detailed_queue_messages_ready{queue="clicks"} > 10000` | 10m | warning |
  | `PublisherDisconnected` | `shortlink_rabbitmq_publisher_connected == 0` | 2m | warning |
  | `ExpiryScanStale` | `time() - shortlink_worker_expiry_scan_last_success_timestamp_seconds > 900` | 0m | warning |

  (`900` = 3 × the default 5m interval; documented.) Every alert has `summary` and `description` annotations.
- Makefile:
  - `observe`: `docker compose -f docker-compose.yml -f docker-compose.observability.yml --profile app up -d --build --wait`, then echo the Grafana/Prometheus/Alertmanager URLs.
  - `observe-down`: same files, `down` (keep volumes).
  - `observe-check`: run `promtool check config`, `promtool check rules`, `amtool check-config` and `alloy fmt` inside their images via `docker run --rm -v`.
- `.env.example`: `GRAFANA_ADMIN_PASSWORD`, `PROMETHEUS_RETENTION=15d`, `LOKI_RETENTION=168h`.

- [ ] **Step 1:** Write the configs. `make observe-check` → all four report success.
- [ ] **Step 2:** `make observe`; open `localhost:9090/targets` → every target `UP`.
- [ ] **Step 3:** In Grafana Explore (Loki) `{service="server"}` shows access-log lines; `{service="server"} | request_id="<id from a curl's X-Request-ID>"` returns that request's line.
- [ ] **Step 4:** `docker compose stop mysql`; within ~3 min `DependencyDown` is firing at `localhost:9093`; `docker compose start mysql` → resolves.
- [ ] **Step 5: Commit** `feat(observability): Prometheus, Alertmanager, Loki, Alloy, Grafana stack with alert rules`.

---

### Task 8: Grafana dashboards

**Files:**
- Create: `deploy/observability/grafana/dashboards/{service-overview,worker-queues,dependencies,logs}.json`

**Decisions:**
- Datasource refs by UID `prometheus` / `loki` only. `editable: false`, fixed `uid` per dashboard (`sl-overview`, `sl-worker`, `sl-deps`, `sl-logs`), refresh `30s`, default range last 1h.
- Panels follow spec §8. Use these queries as the canonical ones (others are the builder's call):
  - RPS: `sum by (route) (rate(shortlink_http_requests_total[$__rate_interval]))`
  - Error rate: the `HighErrorRate` ratio with `[$__rate_interval]`.
  - p50/p95/p99 per route: `histogram_quantile(0.95, sum by (le, route) (rate(shortlink_http_request_duration_seconds_bucket[$__rate_interval])))`
  - Cache hit rate: `sum(rate(shortlink_link_cache_requests_total{result="hit"}[$__rate_interval])) / sum(rate(shortlink_link_cache_requests_total[$__rate_interval]))`
  - Queue depth: `rabbitmq_detailed_queue_messages_ready`
  - Logs: `{service=~"$service", level="error"}`; request trace panel: `{env="local"} | request_id="$request_id"` with a text variable `request_id`.
- Overview error-rate panel has a data link to the Logs dashboard with `service` and the time range.
- Community dashboards for mysqld/redis/cAdvisor may be imported and trimmed; keep panels on `uid: prometheus`.

- [ ] **Step 1:** Build the dashboards (it is fine to design in Grafana UI against the running stack, then export JSON with "Export for sharing externally" **off**, and replace datasource refs with the fixed UIDs).
- [ ] **Step 2:** `make observe-down && make observe`; all four dashboards load under folder "shorten-link" with no "datasource not found" errors; after some curl traffic each panel shows data or "No data" only where nothing happened (e.g. DLQ).
- [ ] **Step 3: Commit** `feat(observability): provisioned Grafana dashboards`.

---

### Task 9: k6 load script

**Files:**
- Create: `deploy/observability/k6/load.js`
- Modify: `docker-compose.observability.yml` (service `k6`, `profiles: ["tools"]`, `grafana/k6` pinned image, mounts the script, `BASE_URL=http://server:8080`), `Makefile` (`load` target: `docker compose -f … -f … --profile tools run --rm k6 run /scripts/load.js`)

**Decisions:**
- `setup()`: register a random user (`k6-<timestamp>@example.test`), log in, create 50 links; return codes + token.
- Default scenario `steady`: `ramping-arrival-rate` to `RATE` (env, default 50 req/s) for `DURATION` (env, default 5m). Mix: 90% `GET /{code}` with `redirects: 0` (5% of those unknown codes → 404), 8% `GET /api/links`, 2% `POST /api/links`.
- Every request sets `X-Real-IP` to a random address in `10.0.0.0/8` so the per-IP limits behave like many users (the server trusts `X-Real-IP` from a private peer, which k6 inside the compose network is). Scenario `burst` (env `SCENARIO=burst`): fixed single `X-Real-IP` at 5 req/s for 2m → shows 429s and `shortlink_ratelimit_rejected_total`.
- Creation from many IPs still hits the user's `quota_daily`; keep POST share low and accept 429/403 as expected statuses (`http.setResponseCallback` with 200–404, 410, 429 expected).
- Thresholds: `http_req_failed{expected_response:true}<0.01` and `p(95)<300` for redirects.

- [ ] **Step 1:** Write the script. `make load` (with `make observe` running) completes; k6 summary shows thresholds passed.
- [ ] **Step 2:** Overview dashboard shows ~RATE RPS, cache hit rate near 100% after warm-up, 404s on `GET /{code}`; Worker dashboard shows click batches.
- [ ] **Step 3:** `SCENARIO=burst make load` → 429s visible on the overview and rate-limit panel.
- [ ] **Step 4: Commit** `feat(observability): k6 load script`.

---

### Task 10: Docs and spec status

**Files:**
- Create: `docs/observability.md`
- Modify: `docs/architect.md` (big picture gains the stack; §9 note that observability being down never affects requests), `docs/spec.md` (FR-O3 ✅, NFR-OB2 ✅, NFR-OB3 ✅ local only — VPS not yet; keep NFR-OB1 gap text), `README.md` (`make observe`, `make load`, URLs, default Grafana login), `docs/deploy.md` (short "Observability is local-only for now; see design §9" note), `docs/modules/README.md` (if not already done in Task 1), and remove/redirect the Phase 3 section of `2026-06-23-production-readiness-design.md` with one line "Superseded by 2026-11-20-observability-design.md".

**`docs/observability.md` must cover:** how to start/stop, URLs and login, every metric (name, labels, meaning), each dashboard and what question it answers, each alert with "what to check when it fires", LogQL examples (errors by service, one request by `request_id` across server and worker), the cardinality rule, known gaps (root for mysqld-exporter, cAdvisor on macOS, no external alert receiver, no tracing, `env="local"` hard-coded in Prometheus/Alloy config), and the manual acceptance checklist from spec §10.

- [ ] **Step 1:** Write docs.
- [ ] **Step 2:** Follow the acceptance checklist end-to-end once on a clean `make observe-down -v` (wipe volumes) + `make observe` + `make load`; fix anything the docs get wrong.
- [ ] **Step 3:** `go test ./... -race` and `make lint` → pass.
- [ ] **Step 4: Commit** `docs(observability): runbook, metrics reference, spec status`.
