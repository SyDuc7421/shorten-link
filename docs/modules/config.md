# `config`: settings and connections

**What it does.** Reads every setting from environment variables, and opens the
connections to MySQL, Redis and RabbitMQ. It also loads the RSA key pair used
for JWTs.

**Where it sits.** Only `cmd/server` and `cmd/worker` use it. No other package
reads environment variables; they receive what they need as arguments.

## Settings (`config.go`)

`config.Load()` fills a `Config` struct using
[envconfig](https://github.com/kelseyhightower/envconfig) tags. Missing
**required** variables make it fail at startup with a message naming the
variable, which is better than failing on the first request.

| Variable | Default | Required | Meaning |
|---|---|---|---|
| `PORT` | `8080` | | HTTP port of the server |
| `BASE_URL` | `http://localhost:8080` | | Prefix for `short_url` in responses |
| `ALLOWED_ORIGINS` | empty | | Comma-separated origins allowed by CORS. Empty = none |
| `NODE_ID` | `1` | | Snowflake node ID, 0–1023. **Must differ per server instance** |
| `MYSQL_HOST`, `MYSQL_PORT` | `localhost`, `3306` | | |
| `MYSQL_USER`, `MYSQL_PASSWORD`, `MYSQL_DB` | | ✔ | |
| `REDIS_ADDR` | `localhost:6379` | | |
| `REDIS_PASSWORD` | empty | | Empty = no auth |
| `REDIS_DB` | `0` | | |
| `RABBITMQ_URL` | | ✔ | `amqp://user:pass@host:5672/` |
| `JWT_PRIVATE_KEY_PATH`, `JWT_PUBLIC_KEY_PATH` | | ✔ | PEM files, created by `make keys` |
| `PASSWORD_RESET_URL` | `http://localhost:3000/reset-password` | | GUI page the reset email links to; `?token=` is appended |
| `SMTP_HOST` | empty | | SMTP relay. Empty = reset links go to the log, no mail is sent |
| `SMTP_PORT` | `587` | | |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | empty | | No auth when the username is empty |
| `SMTP_FROM` | `no-reply@localhost` | | Sender address |

`.env.example` lists them all with comments and defaults that work for local
development. `docker-compose.prod.yml` overrides the hostnames and key paths
inside the containers, so the same `.env` layout works in production. Some
variables in `.env.example` (`DOMAIN`, `IMAGE_*`, `RABBITMQ_USER`, …) are read
only by Docker Compose, not by the Go programs.

## Connections (`connect.go`)

| Method | What it does |
|---|---|
| `OpenMySQL(ctx)` | Opens a pool (50 open, 20 idle, connections recycled after 5 min) with `parseTime=true`, then pings. Fails if the ping fails |
| `NewRedisClient(ctx)` | Creates a client with a pool of 100, then pings |
| `DialRabbitMQ()` | Dials once. The server passes this function to the publisher, which calls it again on every reconnect |

## Keys (`keys.go`)

`LoadRSAKeys()` reads the two PEM files. The private key may be PKCS#1 or
PKCS#8; the public key must be PKIX (what `openssl rsa -pubout` writes). Both
must be RSA.

## Before you change it

- **New setting:** add a field with an `envconfig` tag here, and add the
  variable with a comment to `.env.example`. If production needs a different
  value, mention it in [deploy.md](../deploy.md). Then update the table above.
- There are no tests in this package. A typo in a tag shows up as a startup
  error, so run the server once after changing it.
