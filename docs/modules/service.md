# `internal/service`: the business rules

**What it does.** This package is where the application's rules live: what
makes a URL acceptable, how a short code is chosen, when an account is locked,
how tokens rotate. It knows nothing about HTTP, SQL or Redis commands.

**Where it sits.** Handlers call it. It talks to storage only through
interfaces it declares itself, in `link.go`, `auth.go` and `admin.go`:

| Interface | Implemented by | Purpose |
|---|---|---|
| `LinkRepository` | `repository.linkMySQL` | Store and read links |
| `LinkCache` | `repository.linkRedis` | Cache code → URL for fast redirects |
| `Publisher` | `messaging.Publisher` (or `NoopPublisher`) | Send events such as clicks |
| `UserRepository` | `repository.userMySQL` | Store and read users |
| `TokenCache` | `repository.tokenRedis` | Refresh tokens, the per-user session index, login-failure counters, password reset tokens |
| `Mailer` | `mail.SMTPMailer` or `mail.LogMailer` | Send the password reset email |
| `AdminRepository` | `repository.adminMySQL` | Admin lists and audited writes |

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
| `ErrInvalidToken` | A JWT or refresh token is invalid, expired or already used, or the account is disabled (refresh) |
| `ErrAccountDisabled` | An admin disabled the account (login with the right password, link creation) |
| `ErrQuotaExceeded` | The user already created `quota_daily` links today (UTC) |
| `ErrLinkDisabled` | An admin disabled the link or its owner (redirect) |
| `ErrSelfDisable` | An admin tried to disable their own account |
| `ErrInvalidQuota` | `quota_daily` outside 0–100000 |
| `ErrReasonTooLong` | An admin's reason is longer than 500 bytes |

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
5. **Account and daily quota.** `repo.QuotaUsage` returns how many links the
   user created since 00:00 UTC today, their `quota_daily` (100 by default)
   and whether the account is disabled. A disabled account gets
   `ErrAccountDisabled`; this is what stops a disabled user whose access token
   has not expired yet. At or over the quota, `Shorten` returns
   `ErrQuotaExceeded`. "Since midnight" is
   `id >= idgen.MinIDAt(midnight)`: link IDs are time-ordered, so no
   timestamp column or time-zone setting is involved. Deleted links no longer
   count. Two requests running at the same moment can both pass and overshoot
   the quota by a few; the create rate limit (10/min) keeps that small.
6. **Store.** A new Snowflake ID is drawn. The short code is the alias, or the
   ID written in Base62 (see [idgen.md](idgen.md)). Then `repo.Create`.

**Why retry on a taken code?** Generated codes are unique among themselves,
but a user's alias may already occupy a code the generator produces later. When
`repo.Create` returns `ErrCodeTaken` for a generated code, `Shorten` draws a new
ID and tries again, up to 3 attempts. An alias is the user's choice, so it gets
exactly one attempt and the error goes back to the user as `409`.

After a successful insert, the cache is warmed with the URL (TTL 24 h, or less
if the link expires sooner), and a `link.created` event is published.

### Following a link: `Resolve`

1. Look in the cache. If the value is the marker `!disabled`, return
   `ErrLinkDisabled`. On any other hit, publish `link.clicked` and return the
   URL. The database is not touched, which is what keeps redirects fast.
2. On a miss, read the link from MySQL (`GetByCode` also says whether the
   owner is disabled). If the link or its owner is disabled, cache
   `!disabled` for 5 minutes and return `ErrLinkDisabled`; the marker stops
   clicks on a disabled, often abusive, link from each querying MySQL. If the
   link has expired, return `ErrNotFound`. Otherwise warm the cache, publish
   `link.clicked` and return.

No click is published for a disabled link. Whoever disables or enables a link
or a user must delete the affected cache keys (see `AdminService`), otherwise
a cached URL keeps redirecting for up to 24 h.

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
4. On success, clear the counter. If an admin has disabled the account
   (`users.disabled_at` set), return `ErrAccountDisabled`; otherwise issue a
   token pair. The disabled check comes after the password check on purpose:
   only someone who knows the password learns that the account is disabled.

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
After the consume, `Refresh` reads the user: a disabled (or deleted) account
also gets `ErrInvalidToken`.

Every refresh token is also recorded in a per-user index, so
`RevokeAllRefreshTokens` can end all of a user's sessions at once. A password
reset uses it.

**Revoking safely while requests are in flight.** `Login` and `Refresh` note
the time *before* they check the password or consume the old token
(`authorizedAt`) and pass it when storing the new refresh token. A revoke
records its own time, and the store is refused if the revoke happened at or
after `authorizedAt`. So a session authorised by the old password or an old
refresh token can never be created after the reset that was meant to end it.
`Login` reports that case as `ErrInvalidCredentials`, `Refresh` as
`ErrInvalidToken`.

`ParseAccessToken` is what `middleware.JWTAuth` calls. It accepts only RSA
signatures, which blocks the classic "change the algorithm to `none`/HMAC"
attack.

### Password reset

Two steps, matching the two endpoints.

**`RequestPasswordReset(ctx, email)`** returns nothing. It normalises the email
and hands **all** the work to a background goroutine (`runAsync`, 10 s
timeout), so the call takes the same time and has the same outcome for every
address. Doing the lookup or the Redis calls inside the request would make
registered emails measurably slower, and a storage error would turn into a
different status code. Background failures are logged with the request's
logger (`logger.FromContext`), so they carry the request ID.

In the background:

1. Look the user up. Unknown email: stop.
2. Take a **send slot** for the address (one Lua script): refused if a mail
   went out in the last minute, or if 5 already went out in the last 24 hours.
   The per-IP rate limit alone would let someone with many IPs flood a
   victim's inbox; the daily cap does not care how many IPs they have.
3. Generate 32 random bytes (base64url, no padding) and store only their
   SHA-256, for 30 minutes. Storing a new token invalidates the user's previous
   one, so only the latest link works. If storing fails, the slot is given
   back, so the user's retry is not silently swallowed by the cooldown.
4. Build the link (`resetURL` + `?token=`) and mail it.

**`ResetPassword(ctx, token, password)`**:

1. Check the password length **first**, so a weak password does not use up the
   link. The user fixes it and submits again.
2. Consume the token by its hash (atomic, single use). Unknown, expired or used:
   `ErrInvalidToken`.
3. Load the user and bcrypt the new password.
4. **Revoke all sessions**, then update the password hash, then **revoke again**
   (retried up to 3 times). The first revoke means that if anything later
   fails, the existing sessions are already gone. The second catches a login
   with the old password or a refresh that was in flight during the change.
5. Clear the lockout counter and the reset pointer.

If the first revoke fails, the password is left unchanged and the call fails.
In every failure case the token is already consumed and the user requests a new
link. Putting it back would let a failing request be retried forever.

Two limits worth knowing: an access token issued before the reset stays valid
until it expires (at most 15 minutes), and refresh tokens issued before the
per-user index existed are not revoked.

## Admin (`admin_service.go`)

`AdminService` backs `/api/admin/*` and the `server admin` command. The
repository does the auditing (each write and its `audit_log` row are one
transaction); the service validates input and keeps sessions and the redirect
cache consistent with the database.

- **`IsActiveAdmin`** reads the user on every call: role `admin` and not
  disabled. `RequireAdmin` uses it, so revoking or disabling an admin takes
  effect on their next request, not when their token expires.
- **Validation:** `reason` at most 500 bytes (`ErrReasonTooLong`), quota
  0–100000 (`ErrInvalidQuota`). Checked before anything is written.
- **`DisableUser`** refuses `actorID == userID` (`ErrSelfDisable`), then:
  1. marks the user disabled (audited),
  2. revokes all refresh tokens (`RevokeAllRefreshTokens`),
  3. deletes the cache keys of all their links (`DeleteMany`), so the next
     click reads the disabled state from MySQL.

  Steps 2 and 3 run even if the user was already disabled, so calling it again
  after a failure in step 2 or 3 finishes the job. The user's access token
  still works for up to 15 minutes, but login, refresh, link creation and the
  admin API all check the database.
- **`EnableUser`** clears the flag and purges the cache again, which removes
  the `!disabled` markers. Links an admin disabled one by one stay disabled.
- **Known gaps (cache races, all milliseconds wide, accepted):**
  - A redirect that missed the cache and read the row just before a
    disable can write the old URL back after the purge; it then redirects
    until the cache TTL (up to 24 h).
  - The reverse for enable: a redirect that read the row just before the
    enable can write `!disabled` after the purge, so the link answers 410 for
    up to 5 minutes.
  - A link the user creates while being disabled can be inserted after
    `DisableUser` listed their codes; `Shorten` then warms the cache with its
    URL and it redirects for up to 24 h. A second purge later would close all
    three if they are ever seen in practice.
- **Link disable/enable/delete** write through the repository, then delete
  `sl:<code>`.
- **`GetUser`** adds links created today (UTC, via `idgen.MinIDAt`) and total
  clicks.
- **`SetAdmin(email, admin)`** is for the CLI: it lower-cases the email and
  sets the role with actor `0` (system).

## Before you change it

- **New rule = new test first.** The mocks at the top of `link_service_test.go`
  and `auth_service_test.go` (`mockRepo`, `mockCache`, …) let you control what
  storage returns.
- **New error:** declare it in `link.go` or `auth.go` with a comment, return it
  wrapped, and map it in the handler.
- `TestShorten_OK` and the SSRF tests resolve real hostnames such as
  `example.com`, so they need network access.
- Not built yet: logout and email verification (FR-A8, FR-A10 in
  [spec.md](../spec.md)). Revoking all sessions exists (`RevokeAllRefreshTokens`)
  but only a password reset triggers it; there is no endpoint for it yet.
- The password reset tests use `mockMailer`, which records mails on a channel
  because they are sent from a goroutine. `next` waits for one; `none` checks
  that nothing arrives within 100 ms.
- Lockout has two known gaps. The check and the increment are separate steps,
  so parallel login attempts that all pass the check before any increment can
  try more than 5 passwords. And `INCR` and `EXPIRE` are separate Redis calls,
  so a crash between them leaves a counter that never expires (NFR-R7 in
  spec.md).
