# `internal/metrics`: Prometheus metrics

**What it does.** Owns the Prometheus registry and every metric the server and
worker export, and serves them on `/metrics` from a separate port.

**Where it sits.** `cmd/server` and `cmd/worker` call `metrics.Serve` at start-up.
Other packages (middleware, handlers, messaging) import this package and update
the exported variables. Nothing here imports the rest of the app. The HTTP
metrics are updated here, by the middleware in `http.go`; the others are updated
by `service`, `middleware` (rate limit) and `messaging`. What each metric means,
and the dashboards and alerts built on them, are in
[../observability.md](../observability.md).

## How it works

- `Registry` is a private `prometheus.Registry` with the Go runtime and process
  collectors plus every metric below, registered in `init()`.
- `Serve(ctx, port, log)` does nothing when `port` is `0` (the default). Otherwise
  it binds `:port` immediately, so a clash is returned as an error and the caller
  should exit. It serves `GET /metrics` in the background and shuts down (5s
  grace) when `ctx` is cancelled.
- `RegisterPublisherConnected(fn)` adds the gauge `shortlink_rabbitmq_publisher_connected`
  (1/0). Call it once per process.
- `Middleware(next)` is the outermost HTTP wrapper in `cmd/server`. It puts a
  route holder in the request context, records the status (default 200), and
  after the request updates `HTTPRequests`, `HTTPDuration` and `HTTPInFlight`.
  `method` is the request method if it is GET, POST, PUT, PATCH, DELETE, HEAD or
  OPTIONS, otherwise `OTHER`.
- `RecordRoute(mux, next)` sits just inside `TimeoutHandler`. It reads the
  pattern `mux` would match with `mux.Handler(r)`, stores it in the holder, then
  calls `next` (the rest of the chain, ending in the mux). The holder is filled
  before anything else runs, so a request cut off by `TimeoutHandler` or
  rejected early (429, 413) still has its route. The mux is never served from
  here, so `r.PathValue` is populated by the mux inside `next`. The holder is atomic because `TimeoutHandler`
  runs the inner handler in another goroutine. No pattern means `route="unmatched"`.
- Updating a metric never blocks and never returns an error.

## Metrics

| Variable | Type | Name | Labels |
|---|---|---|---|
| `HTTPRequests` | counter | `shortlink_http_requests_total` | method, route, status |
| `HTTPDuration` | histogram | `shortlink_http_request_duration_seconds` | method, route |
| `HTTPInFlight` | gauge | `shortlink_http_requests_in_flight` | |
| `CacheRequests` | counter | `shortlink_link_cache_requests_total` | result: hit, miss, error |
| `Redirects` | counter | `shortlink_redirects_total` | outcome: found, not_found, disabled, error |
| `LinksCreated` | counter | `shortlink_links_created_total` | |
| `Logins` | counter | `shortlink_auth_logins_total` | result: ok, fail, locked |
| `RateLimited` | counter | `shortlink_ratelimit_rejected_total` | action |
| `Publishes` | counter | `shortlink_rabbitmq_publish_total` | routing_key, result: ok, error |
| `WorkerMessages` | counter | `shortlink_worker_messages_total` | queue, result: ack, nack, dlq |
| `ClickBatchSize` | histogram | `shortlink_worker_click_batch_size` | |
| `ClickFlushDuration` | histogram | `shortlink_worker_click_flush_duration_seconds` | |
| `ClickFlushes` | counter | `shortlink_worker_click_flush_total` | result |
| `ExpiryScans` | counter | `shortlink_worker_expiry_scan_total` | result |
| `ExpiryScanLastSuccess` | gauge | `shortlink_worker_expiry_scan_last_success_timestamp_seconds` | |
| (via `RegisterPublisherConnected`) | gauge | `shortlink_rabbitmq_publisher_connected` | |

## Adding a metric

1. Declare it as an exported `var` in `metrics.go` with the `shortlink_` prefix.
2. Add it to the `MustRegister` call in `init()`.
3. Add a row to the table above.

## Why it is built this way

- A separate port keeps `/metrics` off the public listener and out of nginx.
- A private registry means only metrics declared here are exposed.

## Before you change it

- **Cardinality rule:** never use a short code, user ID, request ID, email or raw
  URL path as a label value. Labels must have a small, fixed set of values.
- Tests (`server_test.go`) use the real listener on a free port. Because metrics
  are package-level, tests that assert counters should compare before/after deltas.
