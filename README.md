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
`RABBITMQ_URL` in your `.env` to match (the user is only created on first start
of an empty data directory, so recreate with a fresh container):

```bash
docker compose up -d --force-recreate rabbitmq
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
Desktop and OrbStack usually work as-is). Both containers serve Prometheus
metrics on port 9100 inside the compose network; it is not published to the host.

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
