# `cmd/server`: the HTTP server

**What it does.** `cmd/server/main.go` is the entry point of the API server. It
contains almost no logic of its own. Its job is to build every other piece, plug
them together, and run the HTTP server until it receives a shutdown signal.

**Where it sits.** It is the only place, apart from `cmd/worker`, that imports
every package. Nothing imports it.

## The `admin` command (`admin_cli.go`)

`server admin create <email>` makes an admin: it creates the account with
role `admin` (password from the `ADMIN_PASSWORD` environment variable, 8–72
bytes, bcrypt like registration) or, if the email exists, only grants the role
and keeps the password. It is the way to seed the first admin (`make
seed-admin` locally). `server admin grant <email>` and `server admin revoke
<email>` set an existing user's role. All of them exit without starting HTTP. `main` checks `os.Args[1] == "admin"`
before anything else. The command loads the normal config, so every required
variable must be set (run it in the server's container, which has them), but
it connects only to MySQL, so it works while Redis or RabbitMQ are down. Exit codes: `0` done
(also "already an admin"), `1` unknown email, weak or missing password for a
new account, or database error, `2` bad usage. The password is never printed.
The change is audited with actor `0`. In production run it with
`docker compose run --rm --no-deps server admin ...` (see
[deploy.md](../deploy.md)); locally `make seed-admin` or `make admin-grant EMAIL=...`.

## Startup, step by step

`main()` runs these steps in order. If a step marked *fatal* fails, the process
exits and Docker restarts it.

| Step | What happens | If it fails |
|---|---|---|
| 1 | `config.Load()` reads environment variables | exit 1 |
| 2 | `logger.New()` creates a JSON (zap production) logger | exit 1 |
| 3 | `cfg.OpenMySQL` opens a pool and pings MySQL | fatal |
| 4 | `cfg.NewRedisClient` connects and pings Redis | fatal |
| 5 | `messaging.NewPublisher` tries to connect to RabbitMQ | **not fatal**: it keeps retrying in the background |
| 6 | `cfg.LoadRSAKeys` reads the JWT key pair | fatal |
| 7 | `idgen.NewGenerator(cfg.NodeID)` | fatal if `NODE_ID` is outside 0–1023 |
| 8 | `PASSWORD_RESET_URL` is parsed and checked. The mailer is SMTP when `SMTP_HOST` is set, otherwise log-only, which is allowed only when the reset URL is on localhost | fatal |
| 9 | Repositories → services → handlers are constructed (including `AdminService`, which `RequireAdmin` also uses) | — |
| 10 | Routes are registered on one `http.ServeMux` | — |
| 11 | The mux is wrapped in rate limits, then in the middleware chain | — |
| 12 | `ListenAndServe` starts in a goroutine | fatal |

RabbitMQ is the only dependency the server can start without. Redirects and link
creation must keep working when the click pipeline is down. Click counting is
"best effort" (see [messaging.md](messaging.md)).

## The request pipeline

Every request passes through these layers, outermost first:

```
http.TimeoutHandler (10 s)          → 503 {"error":"request timeout"} if a handler is too slow
  SecureHeaders                     → security headers on every response
    CORS                            → answers preflight, adds CORS headers for allowed origins
      JWTAuth                       → if a valid Bearer token is present, puts the user ID in the context
        RequestID                   → X-Request-ID header + request-scoped logger
          AccessLog                 → one "request" log line per request, after it finishes
            Recover                 → handler panic → logged error + 500 {"error":"internal server error"}
              MaxBodySize (1 MB)    → caps the request body
                withRateLimits      → per-IP limits on selected routes
                  ServeMux          → the route handlers
```

Two details that surprise people:

- **`JWTAuth` never rejects a request.** A missing or invalid token only means
  "no user in the context". Rejection happens later, in `RequireAuth`, which
  wraps only the protected routes. See [middleware.md](middleware.md).
- **The order matters.** The timeout is outermost so it also bounds slow
  middleware. Rate limiting is innermost so a 429 still carries the security
  headers and the request ID.

## Rate limits

`withRateLimits` decides which limiter (if any) applies, based on method and
path. Each limiter is a sliding window per client IP, stored in Redis.

| Route | Limit per IP | Why |
|---|---|---|
| `GET /{code}` (any GET that is not `/healthz` or `/api/...`) | 60 / minute | Redirects are public |
| `POST /api/links` | 10 / minute | Creating links is the main abuse vector |
| `GET /api/links/availability` | 60 / minute | Called while typing; also stops bulk probing of codes |
| `POST /api/auth/login` | 10 / minute | Each attempt runs bcrypt (~0.25 s of CPU) |
| `POST /api/auth/register` | 5 / hour | Same, and new accounts are cheap to abuse |
| `POST /api/auth/refresh` | 30 / minute | — |
| `POST /api/auth/forgot-password` | 5 / hour | Each accepted request can send a mail |
| `POST /api/auth/reset-password` | 10 / minute | Each attempt runs bcrypt |

Everything else (listing, deleting, `/healthz`, `/docs`) is not rate limited in
the app. nginx adds a coarse 100 requests/second limit in front of everything.

## Shutdown

On `SIGINT` or `SIGTERM` the server stops accepting new connections and gives
in-flight requests up to 30 seconds to finish (`srv.Shutdown`). Deferred calls
then close the publisher, Redis and MySQL.

The `http.Server` itself has timeouts too (`newServer`): 5 s to read headers,
10 s to read the body, 15 s to write the response, 60 s idle. The write timeout
is deliberately longer than the 10 s `TimeoutHandler`, so the handler's own 503
body can still be written.

## Before you change it

- **Adding a route?** Register it in its handler's `Register` method, not here.
  Then decide whether it needs a rate limit and add a `case` to `withRateLimits`.
- **Adding a dependency?** Construct it here and pass it down. Packages below
  `cmd/` should never read environment variables or create clients themselves.
- Tests: `cmd/server/ratelimit_test.go` checks every limit against a real Redis
  (testcontainers, so Docker must be running). `main_test.go` checks the server
  timeouts.
