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
  `Sale` and `sale` are different codes (see [database.md](database.md)).
- **`Delete`** runs `DELETE ... WHERE short_code = ? AND user_id = ?`. If no row
  was deleted, it returns `ErrNotFound`. This one query both checks ownership
  and deletes, so another user's link looks exactly like a missing one.
- **`ListByUser`** pages with `WHERE user_id = ? AND id > ? ORDER BY id LIMIT ?`
  (keyset pagination). Unlike `OFFSET`, it stays fast on page 1000, and it does
  not skip or repeat rows when links are added between page loads. Snowflake
  IDs grow over time, so ascending ID order is creation order.
- **`BatchUpdateClicks`** is used by the worker. It takes a map of code →
  number of new clicks and runs one `UPDATE links SET clicks = clicks + ?` per
  code inside a single transaction. Either the whole batch is counted or none
  of it is.

## Users in MySQL (`user_mysql.go`)

`Create`, `GetByEmail` and `GetByID`. Emails arrive already normalised
(lower-cased) from the service; the unique key on `email` backs up the
service's "already registered" check.

## Redis (`link_redis.go`, `token_redis.go`)

| Key | Value | Expiry | Used for |
|---|---|---|---|
| `sl:<code>` | original URL | 24 h, or the link's expiry if sooner | Redirect cache |
| `rt:<jti>` | user ID | 7 days | Refresh tokens |
| `lockout:<email>` | failed login count | 15 min after the first failure | Login lockout |

- **`ConsumeRefreshToken`** uses `GETDEL`, which reads and deletes in one atomic
  step. Two concurrent refreshes with the same token cannot both succeed.
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
