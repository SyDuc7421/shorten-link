# `deploy/migrations`: the database schema

**What it does.** Plain SQL files that create and evolve the MySQL schema, in
[golang-migrate](https://github.com/golang-migrate/migrate) format. Each
change is a pair: `NNN_name.up.sql` applies it, `NNN_name.down.sql` reverts it.

**Where it runs.**

- Locally: `make migrate`.
- In production: the `migrate` service in `docker-compose.prod.yml` applies
  pending migrations and exits before `server` and `worker` start.
- In tests: `setupMySQL` in `internal/repository/integration_test.go` runs the
  `up` files it lists against a fresh container.

golang-migrate records the last applied number in a `schema_migrations` table,
so running it twice does nothing the second time.

## Tables

### `users`

| Column | Notes |
|---|---|
| `id` | Snowflake ID, set by the app (not auto-increment) |
| `email` | Unique. Stored lower-cased by the service |
| `password_hash` | bcrypt |
| `plan`, `quota_daily` | `free` / 100. Links a user may create per UTC day; checked in `LinkService.Shorten` |
| `role` | `user` (default) or `admin`. Only admins may call `/api/admin/*` |
| `disabled_at` | `DATETIME`, nullable. Set while an admin has disabled the account |
| `created_at`, `updated_at` | Set by MySQL |

### `links`

| Column | Notes |
|---|---|
| `id` | Snowflake ID, set by the app |
| `user_id` | Owner. Indexed for listing |
| `short_code` | Unique, `VARCHAR(16)`, **case-sensitive** (`utf8mb4_bin`) |
| `original_url` | `TEXT` |
| `clicks` | Running total, updated in batches by the worker |
| `expires_at` | `DATETIME`, nullable, stored in UTC |
| `disabled_at` | `DATETIME`, nullable. Set while an admin has disabled this link |
| `created_at`, `updated_at` | Set by MySQL |

### `analytics_daily`

Clicks per link per UTC day, one row per `(link_id, stat_date)`. The worker
adds to it in the same transaction as `links.clicks`; `/api/admin/stats` reads
it. There is no foreign key, so rows of a deleted link stay (they still count
in daily totals). Days before this table was first written have no rows.

### `audit_log`

One row per admin action, written in the same transaction as the change (see
[repository.md](repository.md)). `user_id` is the admin (`0` for the CLI),
`action` is e.g. `user.disable` or `link.delete`, `resource_type` +
`resource_id` name the target, `metadata` is JSON (reason, before/after,
code). Indexed by `(user_id, created_at)` and, since 006, `(created_at)`.

## History

| # | Change | Why |
|---|---|---|
| 001 | `users` | — |
| 002 | `links` | — |
| 003 | `analytics_daily`, `audit_log` | Planned features |
| 004 | `links.expires_at` → `DATETIME` | `TIMESTAMP` cannot store dates after 2038-01-19, so long expiries failed |
| 005 | `links.short_code` → `utf8mb4_bin` | The default collation ignores letter case: `/Sale` and `/sale` hit the same row, and generated codes differing only in case collided |
| 006 | `users.role`, `users.disabled_at`, `links.disabled_at`, index `audit_log(created_at)` | Admin site: who may administer, and disabling without deleting |

## Writing a migration

1. Use the next number: `007_what_it_does.up.sql` and `.down.sql`.
2. Start the file with a comment that says **why**, not only what. The history
   table above is built from those comments.
3. Write the `down` file even if you hope never to run it. If it can fail on
   real data, say so in a comment (see 004 and 005).
4. Add the `up` file to the list in `setupMySQL` and write a repository test
   that depends on the change.
5. Never edit a migration that has already run anywhere. Write a new one.
6. Update the tables on this page.

Things to watch for in MySQL:

- `ALTER TABLE` on a large table can lock it. That is fine today; on a big
  production table, check first.
- Tables are created with `DEFAULT CHARSET=utf8mb4` and no explicit collation,
  so text columns are **case-insensitive** unless you say otherwise.
