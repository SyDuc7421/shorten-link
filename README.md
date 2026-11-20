# shorten-link

URL shortener in Go: an HTTP API server (JWT auth, short-link CRUD, redirects)
and a worker that counts clicks from RabbitMQ, stores notifications and scans
for expiring links. Storage is MySQL, with Redis
for caching and rate limiting.

- Architecture: [docs/architect.md](docs/architect.md)
- How each module works: [docs/modules/](docs/modules/README.md)
- Requirements: [docs/spec.md](docs/spec.md)
- API: [api/openapi.yaml](api/openapi.yaml), served as Swagger UI at `/docs`
- Working on the code: [CONTRIBUTING.md](CONTRIBUTING.md)

## Run locally

**Requirements:** Go 1.26+, Docker, `openssl`, and
[golang-migrate](https://github.com/golang-migrate/migrate) (for `make migrate`).

The server and worker run on your machine with `go run`. MySQL, Redis and
RabbitMQ run in Docker (`docker-compose.yml`) and publish their ports on
`localhost`.

1. Create `.env`:

   ```bash
   cp .env.example .env
   ```

   The defaults point at `localhost` and work as-is for local dev. Set
   `ALLOWED_ORIGINS` to your frontend's origin if it isn't
   `http://localhost:3000`. `REDIS_PASSWORD` can be left empty locally, which
   turns Redis auth off.

2. Generate the JWT key pair (written to `./keys`, which git ignores):

   ```bash
   make keys
   ```

3. Start MySQL, Redis and RabbitMQ, then apply the migrations:

   ```bash
   make docker-up
   make migrate        # wait a few seconds after docker-up for MySQL to accept connections
   ```

4. Start the server and, in a second terminal, the worker:

   ```bash
   make run                                                  # http://localhost:8080
   set -a && . ./.env && set +a && go run ./cmd/worker      # worker
   ```

5. Check that it works:

   ```bash
   curl http://localhost:8080/healthz     # {"mysql":"ok","rabbitmq":"ok","redis":"ok"}
   open http://localhost:8080/docs        # Swagger UI
   ```

   The RabbitMQ management UI is at http://localhost:15672. Log in with
   `RABBITMQ_USER` / `RABBITMQ_PASSWORD` from `.env` (`guest` / `guest` if unset).

6. To use the admin API (`/api/admin/*`), set `ADMIN_EMAIL` and
   `ADMIN_PASSWORD` in `.env`, then:

   ```bash
   make seed-admin      # creates the admin, or grants the role if the account exists
   ```

   For an account you already registered: `make admin-grant EMAIL=you@example.com`.

### RabbitMQ credentials

The local RabbitMQ user is `RABBITMQ_USER` / `RABBITMQ_PASSWORD` from `.env`,
defaulting to `guest` / `guest`, so an existing `.env` keeps working.
`.env.example` now sets `shortlink` / `changeme_rabbit` and a matching
`RABBITMQ_URL`. If you set or change them, recreate RabbitMQ and update
`RABBITMQ_URL` in your `.env` to match. The `-V` matters: the image keeps its
data in an anonymous volume, and RabbitMQ only creates the default user when
that volume is empty, so a plain `--force-recreate` keeps the old user.

```bash
docker compose up -d --force-recreate -V rabbitmq
```

### Run the app in containers

Instead of `go run`, the server and worker can run in containers too (profile
`app`). It runs the migrations, then starts both, with the in-network hosts
and the keys from `./keys` mounted read-only. Needs `make keys` first. Stop host
`make run` first, since both use port 8080. Published ports bind `127.0.0.1` only.

```bash
docker compose --profile app up -d --build --wait
curl http://localhost:8080/healthz
docker compose --profile app down        # stop (keeps the data volumes)
```

The containers run as uid 65532, so `keys/private.pem` must be readable by it
(on Linux: `chmod 640` and `chown :65532`, as in the deploy steps below; Docker
Desktop and OrbStack usually work as-is). Inside the containers `METRICS_PORT` is always 9100, whatever `.env` says. Both serve Prometheus
metrics on port 9100 inside the compose network; it is not published to the host.

### Observability stack (metrics, logs, alerts)

`docker-compose.observability.yml` adds Prometheus, Alertmanager, Loki, Alloy,
Grafana, cAdvisor and the MySQL/Redis exporters on top of the `app` profile.
Needs `make keys` first and port 8080 free, like the section above.
The full runbook (every metric, dashboard and alert, LogQL examples, known
gaps, acceptance checklist) is [docs/observability.md](docs/observability.md).

```bash
make observe-check   # validate the Prometheus, Alertmanager and Alloy configs
make observe         # build and start app + dependencies + stack, print the URLs
make observe-down    # stop everything (keeps the data volumes)
make load            # 5 min of mixed traffic through k6 (needs make observe running)
```

For empty metrics, logs and Grafana state, run `make observe-down`, then
`docker volume rm shorten-link_prometheus_data shorten-link_loki_data shorten-link_grafana_data`,
then `make observe`. Do not use `down -v`: it also deletes `mysql_data`, your local database.

`make load` runs `deploy/observability/k6/load.js` in a `grafana/k6` container on
the compose network. It registers a throwaway user, creates 50 links, then ramps up
to `RATE` requests/s (default 50) and holds it for `DURATION` (default 5m): 90% redirects
(5% of them unknown codes, so 404), 8% `GET /api/links`, 2% `POST /api/links`. Each
request sends a random `X-Real-IP` in `10.0.0.0/8`, so the per-IP limits act like many
users. The user's daily quota (`quota_daily`, default set in `internal/service/auth_service.go`)
runs out during long runs; those 429s are expected. The run fails (non-zero exit) if fewer than
99% of checks pass (any 5xx or connection error fails one), if redirect p95 is 300 ms or more,
or if k6 drops iterations because it cannot keep up with `RATE`.

```bash
RATE=100 DURATION=1m make load   # different rate / length
SCENARIO=burst make load         # one IP at 5 req/s for 2 min: shows 429s and shortlink_ratelimit_rejected_total
```

- Grafana <http://localhost:3000>, user `admin`, password `GRAFANA_ADMIN_PASSWORD`
  from `.env` (default `admin`). Loki logs are under Explore, e.g.
  `{service="server"} | request_id="<X-Request-ID of a response>"`.
- Dashboards (Grafana folder `shorten-link`, provisioned and read-only, JSON in
  `deploy/observability/grafana/dashboards/`): Service overview, Worker and queues,
  Dependencies and host, Logs (has a `request_id` box to trace one request).
- Prometheus <http://localhost:9090> (targets, alert rules), Alertmanager
  <http://localhost:9093> (firing alerts; locally there is no notification target).
- Retention: `PROMETHEUS_RETENTION` (default `15d`) and `LOKI_RETENTION` (default `168h`).
- Alert rules live in `deploy/observability/prometheus/rules/shortlink.yml`; thresholds
  are starting values. `ExpiryScanStale` (900s) is 3 x the default `EXPIRY_SCAN_INTERVAL`.

Known gaps: `external_labels.env` in `deploy/observability/prometheus/prometheus.yml`
is hard-coded to `local` (Prometheus does not expand env vars there; change it on
the VPS), and mysqld-exporter connects as MySQL `root` (on the VPS use a dedicated
user with `PROCESS, REPLICATION CLIENT, SELECT`). On macOS (Docker Desktop,
OrbStack) cAdvisor sees the Linux VM, so some container names may be partial.

### Tests

```bash
make test        # needs Docker: integration tests start MySQL/Redis via testcontainers
make lint        # golangci-lint
```

## Deploy (single VPS)

`docker-compose.prod.yml` runs the whole stack on one Linux host with Docker
Compose v2. Only nginx publishes ports (80/443). MySQL, Redis and RabbitMQ stay
on the internal network. Migrations run automatically before the server and
worker start.

Short version. See [docs/deploy.md](docs/deploy.md) for the full runbook
(file permissions, TLS renewal, rollback):

```bash
git clone <repo> shorten-link && cd shorten-link
cp .env.example .env
# edit .env: DOMAIN, BASE_URL=https://<DOMAIN>, ALLOWED_ORIGINS,
# and long random values for every *_PASSWORD
# (no @ : / ? # % in MYSQL_PASSWORD; the migrate URL doesn't escape them)

make keys
chmod 644 keys/public.pem
chmod 640 keys/private.pem && sudo chown :65532 keys/private.pem   # containers run as uid 65532

# first TLS certificate (DNS A record for DOMAIN must point at this host)
set -a && . ./.env && set +a
docker compose -f docker-compose.prod.yml run --rm -p 80:80 certbot \
  certonly --standalone -d "$DOMAIN" --agree-tos -m you@example.com --non-interactive

docker compose -f docker-compose.prod.yml up -d --build --wait
curl https://$DOMAIN/healthz
```

Deploy a new version:

```bash
git pull && docker compose -f docker-compose.prod.yml up -d --build --wait
```

`MYSQL_HOST`, `REDIS_ADDR`, `RABBITMQ_URL` and the `JWT_*` paths can keep
their local values; the prod compose file overrides them with the in-network
hostnames.
