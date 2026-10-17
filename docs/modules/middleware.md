# `internal/middleware`: code around every request

**What it does.** Each file is one HTTP middleware, a function that wraps a
handler and runs before (and sometimes after) it. They handle concerns that
apply to many routes, so individual handlers stay focused on their own job.

**Where it sits.** `cmd/server` composes them into the request pipeline (see
[server.md](server.md#the-request-pipeline)). They depend only on
`internal/ctxkey`, `internal/logger` and the Redis client.

## The middlewares

### `JWTAuth` and `RequireAuth` (`auth.go`)

Authentication is split into two steps on purpose:

1. **`JWTAuth`** runs on every request. If the `Authorization` header is
   `Bearer <token>` and the token is valid, it stores the user ID in the request
   context (`ctxkey.SetUserID`). Otherwise it does nothing and the request
   continues as anonymous.
2. **`RequireAuth`** wraps only protected routes. If the context has no user
   ID, it answers `401 {"error":"unauthorized"}` and stops.

3. **`RequireAdmin(checker)`** wraps the `/api/admin/*` routes, after
   `RequireAuth`. It asks `checker.IsActiveAdmin(userID)` (implemented by
   `service.AdminService`, which reads the user from MySQL) on **every**
   request: `403 {"error":"forbidden"}` unless the user is an admin and not
   disabled, `500` if the lookup fails. The role is deliberately not in the
   JWT: a claim would keep working for up to 15 minutes after an admin is
   revoked or disabled.

This split lets public and protected routes share one pipeline. A public route
such as a redirect never fails because someone sent an expired token.

`JWTAuth` does not know about JWT libraries. It calls a `TokenParser` interface
(`ParseAccessToken(token) (userID, error)`), which `service.AuthService`
implements.

### `RateLimit` (`ratelimit.go`)

`RateLimit(rdb, action, limit, window)` returns a middleware that allows at most
`limit` requests per `window` per client IP.

**How it counts.** A Lua script runs atomically in Redis on the key
`rl:<action>:<ip>`, which is a sorted set of request timestamps:

1. Remove timestamps older than `now - window`.
2. If the number left is already at `limit`, refuse.
3. Otherwise add `now` and extend the key's expiry.

This is a true sliding window: there is no burst at the start of each minute as
there would be with fixed buckets.

**When refused**, it answers `429 {"error":"rate limit exceeded"}` with a
`Retry-After` header set to the window length in seconds.

**Which IP?** `clientIP` uses the TCP peer address. It trusts the `X-Real-IP`
header only when the peer is a private or loopback address, which in
production means "the request came through our nginx". `X-Forwarded-For` is
never trusted, because nginx appends to whatever value the client sent, so a
client could spoof it.

**Fail open.** If Redis is unreachable, the request is allowed. A Redis outage
must not take redirects down with it.

### `CORS` (`cors.go`)

Browsers only let a page on another origin call the API if the response
carries the right CORS headers. `CORS(allowedOrigins)` adds them only when the
request's `Origin` exactly matches one of `ALLOWED_ORIGINS` (case-insensitive,
trailing slash ignored). With an empty list, no cross-origin call is allowed.

Every `OPTIONS` request (a browser "preflight") is answered with `204`
immediately and never reaches a handler.

### `RequestID` (`requestid.go`)

Reuses the incoming `X-Request-ID` header, or generates a random 32-hex-character
ID. The ID is echoed back in the response header, and a logger tagged with
`request_id` is put in the context. Code further down can log with
`logger.FromContext(ctx)` so every line for one request carries the same ID.
`AccessLog` and `Recover` do; handlers and services do not yet.

### `AccessLog` (`accesslog.go`)

After the request finishes, writes one Info line with message `request` and the
fields `method`, `path`, `status`, `bytes`, `duration`, `ip` (same rule as
`RateLimit`, see below) and `user_id` when a valid token was sent. The query
string is left out because it can carry user input. It runs inside `RequestID`,
so the line has `request_id`.

It learns the status by wrapping the `ResponseWriter` (`statusRecorder`, which
`Recover` reuses). Known gaps:

- A request cut off by `http.TimeoutHandler` is logged with whatever status the
  handler wrote, not the 503 the client got, because the timeout sits outside.
- `/healthz` is logged too, so Docker health checks add a line every interval.

### `Recover` (`recover.go`)

Without it, a panic in a handler makes net/http drop the connection and print
a plain-text stack to stderr. `Recover` catches the panic, logs `panic
recovered` at Error level with the panic value and a stack trace (and
`request_id`), and answers `500 {"error":"internal server error"}`. The client
can quote the `X-Request-ID` header to find the log line.

If the handler had already written a status, that status stays and nothing is
appended; only the log line is written. `http.ErrAbortHandler` is re-panicked,
because that is how code deliberately aborts a response in net/http.

### `SecureHeaders` (`secureheaders.go`)

Adds `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, a strict
`Referrer-Policy`, HSTS (2 years), a `Permissions-Policy` that disables
geolocation, camera and microphone, and `Content-Security-Policy:
default-src 'none'`. The API serves JSON, not pages, so nothing needs to load.
The one exception is `/docs`, whose handler replaces the CSP with one that
allows the Swagger UI assets (see [handler.md](handler.md#docs)).

### `MaxBodySize` (`bodysize.go`)

Wraps the body in `http.MaxBytesReader` with a 1 MB limit. A handler that reads
past the limit gets an error; `IsBodyTooLarge(err)` recognises it so the
handler can answer `413` rather than `400`.

## Before you change it

- **A new middleware** goes in its own file with its own test file. Decide
  where it belongs in the chain in `cmd/server/main.go` and say why in a comment.
- `RequireAuth` writes its 401 with `http.Error`, which replaces the
  `Content-Type` with `text/plain`. The body is JSON, but the header says
  otherwise. Clients that check the header will notice.
- Tests: `auth_test.go`, `requestid_test.go`, `accesslog_test.go` and
  `recover_test.go` are plain unit tests.
  `ratelimit_test.go` needs Docker (it starts a real Redis).
