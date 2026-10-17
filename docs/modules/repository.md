# `internal/repository`: MySQL and Redis

**What it does.** Implements the storage interfaces that
[service](service.md) declares. This is the only package that writes SQL or
Redis commands.

**Where it sits.** `cmd/server` and `cmd/worker` construct the repositories
and hand them to services or consumers as interfaces. Each constructor returns
the interface type (`service.LinkRepository`), not the struct, so callers
cannot reach past it.

| File | Implements | Backed by |
|---|---|---|
| `link_mysql.go` | `service.LinkRepository` | `links` table |
| `user_mysql.go` | `service.UserRepository` | `users` table |
| `admin_mysql.go` | `service.AdminRepository` | `users`, `links`, `audit_log` |
| `link_redis.go` | `service.LinkCache` | Redis keys `sl:<code>` |
| `token_redis.go` | `service.TokenCache` | Redis keys `rt:<jti>`, `lockout:<email>` |

## The main job: translating errors

Services must not depend on MySQL or Redis error types, so repositories
translate them:

| Driver result | Becomes |
|---|---|
| `sql.ErrNoRows` | `service.ErrNotFound` |
| MySQL error 1062 (duplicate key) on `links` | `service.ErrCodeTaken` |
| MySQL error 1062 on `users` | `service.ErrEmailTaken` |
| `redis.Nil` (key missing) | `service.ErrNotFound`, or zero for the lockout counter |
| anything else | wrapped: `fmt.Errorf("repository: ...: %w", err)` |

## Links in MySQL (`link_mysql.go`)

- **`Create`** inserts one row. The unique key on `short_code` is what really
  guarantees that two links never share a code; the service's "is it taken?"
  check is only a convenience for the user.
- **`GetByCode`** reads by `short_code`. The column uses a binary collation, so
  `Sale` and `sale` are different codes (see [database.md](database.md)). It
  joins `users` to fill `OwnerDisabled`, so a redirect can refuse every link
  of a disabled user without a second query.
- **`Delete`** runs `DELETE ... WHERE short_code = ? AND user_id = ?`. If no row
  was deleted, it returns `ErrNotFound`. This one query both checks ownership
  and deletes, so another user's link looks exactly like a missing one.
- **`ListByUser`** pages with `WHERE user_id = ? AND id > ? ORDER BY id LIMIT ?`
  (keyset pagination). Unlike `OFFSET`, it stays fast on page 1000, and it does
  not skip or repeat rows when links are added between page loads. Snowflake
  IDs grow over time, so ascending ID order is creation order.
- **`QuotaUsage`** reads the user's `quota_daily` and counts their links with
  `id >= sinceID` in one query. `idx_user_id` stores `(user_id, id)`, so the
  count is an index range scan. An unknown user gives `ErrNotFound`.
- **`BatchUpdateClicks`** is used by the worker. It takes a map of code →
  number of new clicks and runs one `UPDATE links SET clicks = clicks + ?` per
  code inside a single transaction. Either the whole batch is counted or none
  of it is.

## Users in MySQL (`user_mysql.go`)

`Create`, `GetByEmail`, `GetByID` and `UpdatePasswordHash`. Emails arrive already normalised
(lower-cased) from the service; the unique key on `email` backs up the
service's "already registered" check. `UpdatePasswordHash` does not report a
missing user: MySQL counts an unchanged row as 0 affected rows, so that number
cannot tell "no such user" apart from "same hash".

## Admin in MySQL (`admin_mysql.go`)

Backs the admin API (see `AdminService` in [service.md](service.md)).

**Lists** (`ListUsers`, `ListLinks`, `ListAudit`) return at most
`service.AdminPageSize` (20) rows, newest first, with keyset paging on `id`
(`BeforeID` → `id < ?`). Filters are added as `AND` conditions by a small
`filter` helper, so the SQL and its arguments stay in the same order. Text
search is `LIKE '%q%'` with `%`, `_` and `\` in the query escaped
(`containsPattern`); it scans the table, which is fine at the current size.
`ListLinks` status filters:

| Status | Rows |
|---|---|
| `active` | link not disabled, owner not disabled, not expired |
| `disabled` | link disabled **or** owner disabled |
| `expired` | `expires_at` in the past |

**Writes** (`SetUserQuota`, `SetUserDisabled`, `SetLinkDisabled`,
`DeleteLink`, `SetRole`) each run in one transaction: lock the row with
`SELECT ... FOR UPDATE`, change it, and insert the `audit_log` row. If any step
fails, nothing is kept, so there is never a change without its audit entry.
When the row is already in the requested state, nothing is written (no audit
row) and `changed` is `false`. Unknown IDs, codes or emails give
`ErrNotFound`.

`audit_log.metadata` holds `reason` when the admin gave one, `before`/`after`
for a quota change and `code` for link actions; it is `NULL` when there is
nothing to say. `user_id` is the acting admin, `0` for the CLI.

## Redis (`link_redis.go`, `token_redis.go`)

| Key | Value | Expiry | Used for |
|---|---|---|---|
| `sl:<code>` | original URL, or `!disabled` | 24 h, or the link's expiry if sooner; 5 min for `!disabled` | Redirect cache. `DeleteMany` removes many codes with `DEL` in batches of 500 |
| `rt:<jti>` | user ID | 7 days | Refresh tokens |
| `urt:<userID>` | set of the user's refresh token IDs | 7 days, extended on every login or refresh | Revoking all sessions at once |
| `urv:<userID>` | time of the last "revoke all", Unix ms | 7 days | Blocking tokens authorised before that moment |
| `lockout:<email>` | failed login count | 15 min after the first failure | Login lockout |
| `prt:<sha256>` | user ID | 30 min | Password reset tokens (only the hash is stored) |
| `prt-user:<userID>` | hash of the user's current reset token | 30 min | Invalidating the previous link when a new one is requested |
| `prt-cooldown:<email>` | `1` | 60 s | At most one reset mail per minute per address |
| `prt-daily:<email>` | reset mails sent | 24 h from the first | At most 5 reset mails a day per address |

- **`ConsumeRefreshToken`** uses `GETDEL`, which reads and deletes in one atomic
  step. Two concurrent refreshes with the same token cannot both succeed.
- **`StoreRefreshToken`** is a Lua script: it writes `rt:<jti>` and adds the
  jti to `urt:<userID>`, **unless** `urv:<userID>` holds a revoke time at or
  after `authorizedAt`, in which case it stores nothing and returns
  `ErrInvalidToken`. `authorizedAt` is when the password or the previous
  refresh token was checked. Without this check a refresh could consume its old
  token, a password reset could revoke everything, and the refresh would then
  store a fresh token that survives the reset.
- **`RevokeAllRefreshTokens`** is also a Lua script: it records the revoke time
  in `urv:`, then deletes every token in the set and the set itself, in one
  atomic step. It is how a password reset logs the user out everywhere.
  `ConsumeRefreshToken` removes a jti from the set when it is used. The scripts
  build token keys from an argument, which works on one Redis instance but not
  on Redis Cluster. Times come from the app server's clock, so with several
  servers their clocks must agree to within a few milliseconds. Entries for tokens that
  expired on their own can linger in the set until it expires; deleting a key
  that no longer exists is harmless. Tokens issued before the set existed are
  not in it and cannot be revoked this way.
- **`StorePasswordReset`** looks up the user's previous reset token through
  `prt-user:` and deletes it while storing the new one, so only the latest link
  works. `ConsumePasswordReset` uses `GETDEL`, so a link works once even if two
  requests race.
- **`AcquireResetSlot`** is a Lua script: it refuses if the cooldown key
  exists, otherwise increments the daily counter and refuses past the cap,
  otherwise sets the cooldown. `ReleaseResetSlot` deletes the cooldown and
  decrements the counter when the mail was never sent.
- **`IncrLockout`** sets the expiry only on the first increment, so the
  15-minute window starts at the first failure and is not extended by later
  ones.

## Before you change it

- **New query:** write a failing test in `integration_test.go` first. These
  tests start real MySQL 8 and Redis 7 containers (testcontainers), so Docker
  must be running. `make test` runs them; they take about a minute.
- **Why real databases in tests?** Mocks cannot catch mapping bugs. The
  `expires_at` year-2038 bug and the case-insensitive `short_code` bug both
  survived because only mocks covered this code.
- **New migration:** add it to the list in `setupMySQL` in `integration_test.go`,
  or the tests will run against the old schema.
