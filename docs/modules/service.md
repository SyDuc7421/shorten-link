# `internal/service`: the business rules

**What it does.** This package is where the application's rules live: what
makes a URL acceptable, how a short code is chosen, when an account is locked,
how tokens rotate. It knows nothing about HTTP, SQL or Redis commands.

**Where it sits.** Handlers call it. It talks to storage only through
interfaces it declares itself, in `link.go` and `auth.go`:

| Interface | Implemented by | Purpose |
|---|---|---|
| `LinkRepository` | `repository.linkMySQL` | Store and read links |
| `LinkCache` | `repository.linkRedis` | Cache code → URL for fast redirects |
| `Publisher` | `messaging.Publisher` (or `NoopPublisher`) | Send events such as clicks |
| `UserRepository` | `repository.userMySQL` | Store and read users |
| `TokenCache` | `repository.tokenRedis` | Refresh tokens and login-failure counters |

Because the interfaces live here, service tests use small in-file mocks and
run without any database.

## Errors

Every failure the caller should react to is a **sentinel error** declared in
this package. Repositories translate driver errors into them; handlers
translate them into status codes. Always compare with `errors.Is`, because the
service wraps them with context (`fmt.Errorf("service: shorten: %w", err)`).

| Error | Meaning |
|---|---|
| `ErrNotFound` | The link or user does not exist (repositories return it too) |
| `ErrInvalidURL` | Not http/https, unparsable, or the host does not resolve |
| `ErrForbiddenURL` | The URL points at a private or internal address |
| `ErrInvalidExpiry` | `expires_at` is not in the future |
| `ErrInvalidAlias` | The alias has the wrong format or is reserved |
| `ErrCodeTaken` | The short code is already used by another link |
| `ErrEmailTaken` | Registration with an email that already exists |
| `ErrWeakPassword` | Password shorter than 8 or longer than 72 bytes |
| `ErrInvalidCredentials` | Login failed (unknown email *or* wrong password) |
| `ErrAccountLocked` | Too many failed logins for this email |
| `ErrInvalidToken` | A JWT or refresh token is invalid, expired or already used |

## Links (`link_service.go`)

### Creating a link: `Shorten`

`Shorten(ctx, userID, rawURL, alias, expiresAt)` runs these checks in order,
cheapest first, and stops at the first failure:

1. **Scheme.** The URL must parse and use `http` or `https`.
2. **Alias format** (only if `alias` is not empty). 3–16 characters from
   `A-Z a-z 0-9 _ -`, and not one of `api`, `docs`, `healthz`, `metrics` in
   any letter case. Those paths are served by other routes, so a link there
   could never be opened.
3. **Expiry.** If given, it must be in the future.
4. **SSRF guard** (`checkSSRF`). The host must not be `localhost` or an address
   in a private, loopback, link-local or carrier-grade NAT range. Hostnames are
   resolved (2 s timeout) and **every** returned address is checked. A host
   that does not resolve is rejected as `ErrInvalidURL`.
5. **Store.** A new Snowflake ID is drawn. The short code is the alias, or the
   ID written in Base62 (see [idgen.md](idgen.md)). Then `repo.Create`.

**Why retry on a taken code?** Generated codes are unique among themselves,
but a user's alias may already occupy a code the generator produces later. When
`repo.Create` returns `ErrCodeTaken` for a generated code, `Shorten` draws a new
ID and tries again, up to 3 attempts. An alias is the user's choice, so it gets
exactly one attempt and the error goes back to the user as `409`.

After a successful insert, the cache is warmed with the URL (TTL 24 h, or less
if the link expires sooner), and a `link.created` event is published.

### Following a link: `Resolve`

1. Look in the cache. On a hit, publish `link.clicked` and return the URL. The
   database is not touched, which is what keeps redirects fast.
2. On a miss, read the link from MySQL. If it has expired, return
   `ErrNotFound`. Otherwise warm the cache, publish `link.clicked` and return.

Events are published **asynchronously** (`publishAsync`): a goroutine sends
them with a 2 s timeout, and errors are ignored. A slow or absent RabbitMQ
never slows a redirect down, at the cost of losing clicks during an outage.

### Checking an alias: `CheckAlias`

Returns `(available, reason, err)`. It uses the same format and reserved-word
check as `Shorten` (one function, `aliasProblem`), so the two can never
disagree. Then it looks the alias up with `repo.GetByCode`. It reads MySQL, not
Redis, because Redis only holds links that have not expired, while an expired
link still occupies its code.

The answer can go stale: two users can both see "available" and submit at the
same moment. One of them still gets `409` from `Shorten`. That is expected.

### Listing and deleting

`ListByUser` and `Delete` pass straight through to the repository. `Delete`
also removes the code from the cache, so the link stops redirecting
immediately. The repository only deletes a row that belongs to the caller,
which is how ownership is enforced.

## Auth (`auth_service.go`)

### Register

The email is lower-cased and trimmed, so `A@x.com` and `a@x.com` are the same
account. The password must be 8–72 bytes (72 is the most bcrypt will read).
The password is hashed with bcrypt at cost 12, the user is stored on the `free`
plan with a daily quota of 100, and a token pair is issued.

### Login and lockout

1. If this email already has 5 recent failures, refuse with `ErrAccountLocked`,
   **even if the password is right**. This stops password guessing against one
   account.
2. Look the user up. If the email is unknown, compare against a fixed dummy
   hash anyway.
3. On a mismatch, increment the failure counter (it expires 15 minutes after
   the first failure) and return `ErrInvalidCredentials`, or `ErrAccountLocked`
   if this was the fifth failure.
4. On success, clear the counter and issue a token pair.

**Why the dummy hash?** Without it, a login for an unknown email would return
almost instantly, while a real email would take ~0.25 s for bcrypt. That
timing difference would tell an attacker which emails are registered. Unknown
emails also count toward the lockout, for the same reason.

### Tokens

`issueTokenPair` creates two RS256 JWTs signed with the private key:

| Token | Lifetime | Claims | Stored server-side? |
|---|---|---|---|
| access | 15 min | `sub` = user ID | No. Verified with the public key on every request |
| refresh | 7 days | `jti` = random UUID | Yes. `rt:<jti>` → user ID in Redis |

**Refresh rotates.** `Refresh` verifies the JWT, then *consumes* its `jti` from
Redis with an atomic get-and-delete, and issues a brand-new pair. A refresh
token therefore works exactly once, even if two requests race with it. A reused
token is answered with `ErrInvalidToken`, and the user has to log in again.

`ParseAccessToken` is what `middleware.JWTAuth` calls. It accepts only RSA
signatures, which blocks the classic "change the algorithm to `none`/HMAC"
attack.

## Before you change it

- **New rule = new test first.** The mocks at the top of `link_service_test.go`
  and `auth_service_test.go` (`mockRepo`, `mockCache`, …) let you control what
  storage returns.
- **New error:** declare it in `link.go` or `auth.go` with a comment, return it
  wrapped, and map it in the handler.
- `TestShorten_OK` and the SSRF tests resolve real hostnames such as
  `example.com`, so they need network access.
- Not built yet: logout, password reset, email verification and revoking all
  sessions (FR-A8 to FR-A11 in [spec.md](../spec.md)). Refresh tokens are keyed
  by token, not by user, so "log out everywhere" needs a new index first.
- Lockout has two known gaps. The check and the increment are separate steps,
  so parallel login attempts that all pass the check before any increment can
  try more than 5 passwords. And `INCR` and `EXPIRE` are separate Redis calls,
  so a crash between them leaves a counter that never expires (NFR-R7 in
  spec.md).
