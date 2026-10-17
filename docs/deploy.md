# Deploying to a VPS

Runbook for `docker-compose.prod.yml` on a single Linux host with Docker
Compose v2. Only nginx is exposed (ports 80 and 443); MySQL, Redis and RabbitMQ
stay on the internal Docker network.

## 1. First-time setup

```bash
git clone <repo> shorten-link && cd shorten-link
cp .env.example .env
```

Edit `.env`:

| Variable | Set to |
|---|---|
| `DOMAIN` | Public hostname, e.g. `s.example.com` (DNS A record → this VPS) |
| `BASE_URL` | `https://<DOMAIN>` |
| `ALLOWED_ORIGINS` | Your frontend origins, or empty |
| `MYSQL_ROOT_PASSWORD`, `MYSQL_PASSWORD` | Long random values. **Avoid** `@ : / ? # %` in `MYSQL_PASSWORD` (the migrate URL does not escape them) |
| `REDIS_PASSWORD`, `RABBITMQ_PASSWORD` | Long random values |
| `NODE_ID` | `1` (must differ per server instance if you ever run more than one) |
| `PASSWORD_RESET_URL` | `https://<your GUI>/reset-password` (the page the reset email links to) |
| `SMTP_HOST`, `SMTP_PORT` | Your provider's SMTP relay, usually port `587` (STARTTLS). Required in production: with it empty the server refuses to start unless `PASSWORD_RESET_URL` is on localhost |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | SMTP credentials from the provider |
| `SMTP_FROM` | A sender address your provider allows, e.g. `no-reply@<DOMAIN>`. Set up SPF/DKIM for that domain or mail lands in spam |

`MYSQL_HOST`, `REDIS_ADDR`, `RABBITMQ_URL` and the `JWT_*` paths are
overridden by the compose file. You don't need to change them.

Generate the JWT keys. The containers run as uid 65532, so the files must be
readable by that user:

```bash
make keys
chmod 644 keys/public.pem
chmod 640 keys/private.pem && sudo chown :65532 keys/private.pem
```

## 2. TLS certificate (once)

nginx cannot start without a certificate, so issue the first one with certbot
in standalone mode while nothing listens on port 80:

```bash
set -a && . ./.env && set +a      # exports DOMAIN for the commands below
docker compose -f docker-compose.prod.yml run --rm -p 80:80 certbot \
  certonly --standalone -d "$DOMAIN" --agree-tos -m you@example.com --non-interactive
```

## 3. Start

```bash
docker compose -f docker-compose.prod.yml up -d --build --wait
curl https://$DOMAIN/healthz     # {"mysql":"ok","rabbitmq":"ok","redis":"ok"}
```

Startup order is handled by compose: MySQL, Redis and RabbitMQ become healthy →
`migrate` applies `deploy/migrations` and exits → `server` and `worker` start → nginx.

## 4. Routine operations

| Task | Command |
|---|---|
| Deploy a new version | `git pull && docker compose -f docker-compose.prod.yml up -d --build --wait` |
| Logs | `docker compose -f docker-compose.prod.yml logs -f server worker` |
| Renew TLS (cron, daily) | `docker compose -f docker-compose.prod.yml run --rm certbot renew --webroot -w /var/www/certbot && docker compose -f docker-compose.prod.yml exec nginx nginx -s reload` |
| Run migrations only | `docker compose -f docker-compose.prod.yml run --rm migrate` |
| MySQL shell | `docker compose -f docker-compose.prod.yml exec mysql mysql -u root -p` |
| Roll back | `git checkout <previous tag> && docker compose -f docker-compose.prod.yml up -d --build --wait` (down-migrations are manual) |

## 5. Not covered yet

These are tracked in [spec.md §5](spec.md#5-path-to-production-prioritized):

- **Backups.** Data lives in the `mysql-data`, `redis-data` and `rabbitmq-data`
  volumes. Schedule `mysqldump` off the host before launch.
- **Image registry.** Images are built on the VPS. CI only checks that they build.
- **Healthchecks for server/worker containers.** Distroless images have no shell;
  rely on `/healthz` through nginx and on `restart: unless-stopped`.
