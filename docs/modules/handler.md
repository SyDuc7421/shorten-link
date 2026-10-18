# `internal/handler`: HTTP in, HTTP out

**What it does.** Handlers are the thin layer between HTTP and the business
rules. Each one does the same three things:

1. Decode and sanity-check the request (JSON body, path value, query string).
2. Call one service method.
3. Translate the result, or the service's error, into a status code and a JSON body.

Handlers contain **no business rules**. If you find yourself writing an `if`
about links or users in a handler, it belongs in [service](service.md).

**Where it sits.** `cmd/server` builds the handlers and calls their `Register`
methods. Handlers depend on small interfaces they declare themselves
(`LinkServicer`, `AuthServicer`, `ConnStatus`), never on concrete services or
repositories.

## Routes

| Method and path | Auth | Handler | File |
|---|---|---|---|
| `GET /{code}` | — | `LinkHandler.Redirect` | `link.go` |
| `POST /api/links` | ✔ | `LinkHandler.Shorten` | `link.go` |
| `GET /api/links?cursor=` | ✔ | `LinkHandler.List` | `link.go` |
| `GET /api/links/availability?alias=` | ✔ | `LinkHandler.CheckAlias` | `link.go` |
| `DELETE /api/links/{code}` | ✔ | `LinkHandler.Delete` | `link.go` |
| `POST /api/auth/register` | — | `AuthHandler.RegisterUser` | `auth.go` |
| `POST /api/auth/login` | — | `AuthHandler.Login` | `auth.go` |
| `POST /api/auth/refresh` | — | `AuthHandler.Refresh` | `auth.go` |
| `POST /api/auth/forgot-password` | — | `AuthHandler.ForgotPassword` | `auth.go` |
| `POST /api/auth/reset-password` | — | `AuthHandler.ResetPassword` | `auth.go` |
| `GET /api/admin/me` | admin | `AdminHandler.Me` | `admin.go` |
| `GET /api/admin/users` | admin | `AdminHandler.ListUsers` | `admin.go` |
| `GET /api/admin/users/{id}` | admin | `AdminHandler.GetUser` | `admin.go` |
| `PATCH /api/admin/users/{id}` | admin | `AdminHandler.PatchUser` | `admin.go` |
| `POST /api/admin/users/{id}/disable` | admin | `AdminHandler.DisableUser` | `admin.go` |
| `POST /api/admin/users/{id}/enable` | admin | `AdminHandler.EnableUser` | `admin.go` |
| `GET /api/admin/links` | admin | `AdminHandler.ListLinks` | `admin.go` |
| `POST /api/admin/links/{code}/disable` | admin | `AdminHandler.DisableLink` | `admin.go` |
| `POST /api/admin/links/{code}/enable` | admin | `AdminHandler.EnableLink` | `admin.go` |
| `DELETE /api/admin/links/{code}` | admin | `AdminHandler.DeleteLink` | `admin.go` |
| `GET /api/admin/audit` | admin | `AdminHandler.ListAudit` | `admin.go` |
| `GET /api/admin/stats` | admin | `AdminHandler.Stats` | `admin.go` |
| `GET /healthz` | — | `HealthChecker.Health` | `health.go` |
| `GET /docs`, `GET /api/openapi.yaml` | — | `RegisterDocs` | `docs.go` |

Routing uses Go's standard `http.ServeMux` patterns (`"GET /{code}"`). When two
patterns match, the more specific one wins, so `GET /docs` beats `GET /{code}`.
That is why `docs`, `healthz` and friends are reserved and cannot be used as
custom aliases.

## Links (`link.go`)

**Create: `POST /api/links`.** Body: `url` (required), `alias` (optional),
`expires_at` (optional, RFC 3339). Returns `201` with `short_url`, `code` and
`expires_at`. `short_url` is `BASE_URL` + `/` + code.

| Service error | Status | Message |
|---|---|---|
| bad JSON or missing `url` | 400 | `invalid request body` |
| `expires_at` not RFC 3339 | 400 | `expires_at must be RFC3339` |
| `ErrInvalidURL` | 400 | URL must be a valid http(s) URL with a resolvable host |
| `ErrInvalidExpiry` | 400 | `expires_at must be in the future` |
| `ErrInvalidAlias` | 400 | alias format rules |
| `ErrForbiddenURL` | 422 | `URL targets a forbidden address` |
| `ErrCodeTaken` | 409 | `alias already taken` |
| `ErrAccountDisabled` | 403 | `account disabled` |
| `ErrQuotaExceeded` | 429 | `daily link quota exceeded`, with `Retry-After` = seconds until 00:00 UTC |
| body over 1 MB | 413 | `request body too large` |
| anything else | 500 | `failed to shorten URL` |

**Redirect: `GET /{code}`.** Answers `302` with a `Location` header, or `404`
for an unknown, expired or deleted code.

**List: `GET /api/links`.** Returns the caller's links, 20 per page, oldest
first. `next_cursor` is the ID of the last link on the page, or an empty string
on the last page. Pass it back as `?cursor=`.

**Check alias: `GET /api/links/availability?alias=`.** Always `200` when the
check ran. The body is `{"alias", "available", "reason"}`, where `reason` is
`invalid`, `reserved` or `taken` and is omitted when the alias is available.
A missing `alias` parameter is a `400`. "Not available" is a normal answer,
not an error, so a frontend can show the reason under the input box.

**Delete: `DELETE /api/links/{code}`.** `204` on success. `404` if the link does
not exist **or belongs to someone else**: the API does not reveal that another
user's code exists.

The handlers that need a user also check `ctxkey.UserID` themselves, even
though `RequireAuth` already did. That costs nothing and keeps a handler safe if
it is ever registered without the wrapper.

## Auth (`auth.go`)

Register, login and refresh return the same body on success:
`{"access_token": "...", "refresh_token": "..."}`.

| Endpoint | Success | Errors |
|---|---|---|
| register | 201 | 400 missing field or password not 8–72 bytes, 409 email taken |
| login | 200 | 400 missing field, 401 wrong email or password, 403 `account disabled` (only with the right password), 429 account locked |
| refresh | 200 | 400 missing token, 401 invalid, expired or already-used token |
| forgot-password | 202, empty body | 400 missing email or bad JSON. Never 500: the work runs in the background |
| reset-password | 204 | 400 missing field or password not 8–72 bytes, 401 invalid, expired or used token |

**forgot-password answers `202` for every well-formed request**, registered
email or not, so it cannot be used to find out who has an account. It also
returns no body, so there is nothing to compare.

Login answers `401 invalid email or password` for both an unknown email and a
wrong password. That is deliberate (see [service.md](service.md#auth-auth_servicego)). How
the reset itself works is in [service.md](service.md#password-reset).

## Admin (`admin.go`)

Every route is wrapped as `requireAuth(requireAdmin(handler))`, so a handler
only runs for an enabled admin (see `RequireAdmin` in
[middleware.md](middleware.md)). The acting admin is the user ID in the
context; it is passed to the service as `actorID` for the audit log.

**JSON.** IDs are always strings (`"id": "7311..."`): snowflake IDs are larger
than 2^53, and a JavaScript `number` would silently round them. Lists return
`{"users"|"links"|"entries": [...], "next_cursor": "..."}`, 20 rows, newest
first; `next_cursor` is the last row's ID when the page is full and `""` on the
last page. User detail endpoints add `links_today` and `total_clicks`. An audit
entry's `actor` is `null` for the `server admin` command. `GET /stats`
returns `{"totals": {...}, "daily": [{"date": "YYYY-MM-DD", ...}], "top_links": [...]}`;
`days` defaults to 30 and both arrays are `[]`, never `null`.

**Input.** `id`, `cursor`, `user_id` and `actor_id` must be positive integers,
`status` and `resource_type` one of the listed values; anything else is `400`
with a message naming the parameter. Write endpoints take an optional
`{"reason": "..."}` body; an empty body is fine, malformed JSON is `400`.
`PATCH /users/{id}` requires `quota_daily`.

| Service error | Status | Message |
|---|---|---|
| `ErrNotFound` | 404 | `user not found` / `link not found` |
| `ErrSelfDisable` | 409 | `cannot disable yourself` |
| `ErrInvalidQuota` | 400 | `quota_daily must be 0-100000` |
| `ErrReasonTooLong` | 400 | `reason must be at most 500 bytes` |
| `ErrInvalidDays`, or `days` not a number | 400 | `days must be 1-90` |
| body over 1 MB | 413 | `request body too large` |
| anything else | 500 | `admin request failed` |

## Health (`health.go`)

`GET /healthz` pings MySQL and Redis (3 s timeout in total) and asks the
RabbitMQ publisher whether its connection is closed. It returns each status as
`"ok"` or `"down"`, with `200` when all are up and `503` otherwise. Nothing
calls it automatically yet: the distroless image has no shell for a Docker
healthcheck, so it is for humans and external monitors.

Because a RabbitMQ outage makes it return `503` even though redirects still
work, it is not a good liveness probe. Splitting it into liveness and readiness
is tracked as FR-O2 in [spec.md](../spec.md).

## Docs

`RegisterDocs` serves the embedded OpenAPI file at `/api/openapi.yaml` and a
small HTML page at `/docs` that loads a pinned version of Swagger UI from
unpkg. That page sets its own `Content-Security-Policy` to allow those assets,
replacing the strict default from `SecureHeaders`.

The spec is **hand-written** in `api/openapi.yaml`. Changing a handler does not
change the docs; you have to edit the YAML yourself.

## Helpers (`respond.go`)

- `writeJSON(w, status, v)` sets the content type and encodes `v`.
- `writeError(w, status, msg)` writes `{"error": msg}`. Every error response
  in the API has this shape.
- `writeIfTooLarge(w, err)` writes `413` and returns true if `err` came from
  the body-size limit. Call it right after decoding a body.

## Before you change it

- **New endpoint:** add the method to the consumer interface (`LinkServicer` or
  `AuthServicer`), write a failing handler test with the mock in
  `link_handler_test.go` or `auth_handler_test.go`, implement, register the
  route, then update `api/openapi.yaml` and this page.
- **New service error:** map it explicitly. An unmapped error becomes a 500.
- Use `errors.Is`, never `==`. Services wrap their errors (`fmt.Errorf("...: %w", err)`).
- Tests: all handler tests use `httptest` with mock services. No Docker needed.
