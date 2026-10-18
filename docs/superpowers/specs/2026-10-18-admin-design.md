# Admin Site Design

**Date:** 2026-10-18
**Requirements:** FR-L10 (disable abusive links), FR-S2 (per-day clicks), NFR-S12 (audit trail), NFR-R8 (worker update order)
**Repos:** `shorten-link` (Go API) and `shorten-link-gui` (Next.js). This file lives in
`docs/superpowers/specs/` in the API repo; a copy goes to `docs/specs/` in the GUI repo when plan 3 starts.

---

## 1. Goal

One or two operators can sign in to the existing web app and, from an admin
area, find and act on users and links, see how the service is used, and see who
did what.

**Success means:**

- Only users with `role = 'admin'` that are not disabled can call `/api/admin/*`.
  The check reads the database on every request, so revoking an admin takes
  effect immediately.
- A disabled link stops redirecting (410) within one request, from cache or not.
- A disabled user cannot sign in, loses every session, cannot create links, and
  all of their links stop redirecting. Enabling them restores the links, except
  links an admin disabled one by one.
- Every admin write is in `audit_log`, written in the same transaction as the
  change, so no change exists without its log entry.
- The dashboard shows totals and a per-day series (new users, new links, clicks)
  for up to 90 days.

**Out of scope:** user reports of abusive links; Google Safe Browsing; editing a
link's target URL; promoting or demoting admins from the UI; several admin
permission levels; backfilling per-day clicks recorded before this ships;
deleting users.

## 2. Decisions

| Question | Decision | Why |
|---|---|---|
| Where is the UI? | In `shorten-link-gui`, under `/[locale]/admin/*`, same sign-in | Reuses session, components and deploy. Next splits bundles by route, so normal users do not download admin code. |
| How is "admin" checked? | `users.role` read from MySQL on each `/api/admin/*` request | A role claim in the JWT would keep working up to 15 min after revocation. Admin traffic is tiny, one query is cheap. |
| How does the first admin appear? | CLI on the VPS: `server admin grant <email>` / `revoke` | No API can raise privileges, so a stolen user token can never become admin. |
| What does disabling a user do? | Block login, revoke all refresh tokens, refuse link creation, stop all their links | A spammer's links are the problem, not only their account. |
| Disabled link status | `410 Gone`, `{"error":"link disabled"}` | Distinguishes "removed on purpose" from "never existed" (404). |
| IDs in admin JSON | Strings | Snowflake IDs exceed 2^53; JavaScript `number` rounds them. |
| List order and paging | Newest first, keyset cursor, 20 per page | Same mechanism as `GET /api/links`, stays fast on deep pages. |
| Search | `LIKE '%q%'` | Good enough on one VPS. Known gap: full scan; switch to prefix or FULLTEXT if it gets slow. |
| Day boundaries | UTC everywhere; MySQL session `time_zone='+00:00'` via DSN | One definition of "a day" for quota, stats and analytics. |

## 3. Data model

Migration `006_admin`:

```sql
ALTER TABLE users
  ADD COLUMN role        VARCHAR(16) NOT NULL DEFAULT 'user' AFTER plan,
  ADD COLUMN disabled_at DATETIME    NULL;
ALTER TABLE links
  ADD COLUMN disabled_at DATETIME NULL;
ALTER TABLE audit_log
  ADD KEY idx_created (created_at);
```

`role` is `user` or `admin`. `disabled_at` is set when disabled, `NULL` otherwise.

`audit_log` (existing table) is used as:

| Column | Meaning |
|---|---|
| `user_id` | The admin who acted |
| `action` | `user.disable`, `user.enable`, `user.quota`, `link.disable`, `link.enable`, `link.delete`, `admin.grant`, `admin.revoke` |
| `resource_type`, `resource_id` | `user` + user ID, or `link` + link ID |
| `metadata` | JSON: `reason` when given; `before` / `after` for changed values; `code` for links |

The CLI writes `admin.grant` / `admin.revoke` with `user_id = 0` (system).

The DSN in `config/connect.go` gains `time_zone='+00:00'` (URL-encoded) so
`DATE(created_at)` and `CURDATE()` are UTC.

## 4. API (`shorten-link`)

### 4.1 Common rules

- Route order: `RequireAuth` → `RequireAdmin`. No or invalid token: `401`.
  Valid token but not admin, or the admin is disabled: `403 {"error":"forbidden"}`.
- Errors use the existing `{"error":"..."}` shape. Unknown user or link: `404`.
  Bad query or body: `400`.
- All IDs in responses are strings. Times are RFC 3339 UTC.
- Lists: `?cursor=` from the previous `next_cursor` (empty string means last page).
- Writes accept an optional `{"reason": "..."}` (max 500 bytes), stored in the
  audit metadata.

### 4.2 Me

`GET /api/admin/me` → `200 {"id","email","role"}` or `403`. The GUI uses it to
show the admin entry point and to guard admin pages.

### 4.3 Users

| Method | Path | Input | Output |
|---|---|---|---|
| GET | `/api/admin/users` | `q` (email contains), `status=active\|disabled`, `cursor` | `{"users":[User], "next_cursor"}` |
| GET | `/api/admin/users/{id}` | — | `UserDetail` |
| PATCH | `/api/admin/users/{id}` | `{"quota_daily": 0..100000, "reason"?}` | `200 UserDetail`; `400` outside range |
| POST | `/api/admin/users/{id}/disable` | `{"reason"?}` | `200 UserDetail`; already disabled → `200`, no audit row; self → `409 {"error":"cannot disable yourself"}` |
| POST | `/api/admin/users/{id}/enable` | `{"reason"?}` | `200 UserDetail`; already enabled → `200`, no audit row |

`User`: `id, email, role, quota_daily, disabled_at, created_at, link_count`.
`UserDetail`: `User` plus `links_today`, `total_clicks`.

### 4.4 Links

| Method | Path | Input | Output |
|---|---|---|---|
| GET | `/api/admin/links` | `q` (code equals, or URL contains), `user_id`, `status=active\|disabled\|expired`, `cursor` | `{"links":[AdminLink], "next_cursor"}` |
| POST | `/api/admin/links/{code}/disable` | `{"reason"?}` | `200 AdminLink`; idempotent like users |
| POST | `/api/admin/links/{code}/enable` | `{"reason"?}` | `200 AdminLink` |
| DELETE | `/api/admin/links/{code}` | — | `204` |

`AdminLink`: `id, code, short_url, original_url, clicks, owner {id, email, disabled_at}, disabled_at, expires_at, created_at`.

`status=active` means not disabled, owner not disabled, not expired.

### 4.5 Audit

`GET /api/admin/audit?actor_id=&resource_type=user|link&cursor=` →
`{"entries":[{"id","actor":{"id","email"},"action","resource_type","resource_id","metadata","created_at"}], "next_cursor"}`.
`actor` is `null` for system (CLI) entries.

### 4.6 Stats

`GET /api/admin/stats?days=30` (`1..90`, default 30) →

```json
{
  "totals": {"users": 0, "links": 0, "clicks": 0, "disabled_users": 0, "disabled_links": 0},
  "daily": [{"date": "2026-10-18", "new_users": 0, "new_links": 0, "clicks": 0}],
  "top_links": [{"code": "abc", "clicks": 0}]
}
```

`daily` has one entry per day in the range, oldest first, zeros filled in.
`top_links` is the top 10 by clicks within the range (from `analytics_daily`).
Totals come from `COUNT(*)` / `SUM(clicks)` on the live tables.

### 4.7 Changes to existing endpoints

| Endpoint | Change |
|---|---|
| `POST /api/auth/login` | After the password is verified, a disabled user gets `403 {"error":"account disabled"}`. Checked only after the password, so it reveals nothing to someone who does not know it. Failed-attempt lockout is unchanged. |
| `POST /api/auth/refresh` | Disabled user → `401` (same as an invalid token). |
| `POST /api/links` | Disabled user → `403 {"error":"account disabled"}`. Read from the same query as the quota check (adds `disabled_at` to `QuotaUsage`). |
| `GET /{code}` | Link or owner disabled → `410 {"error":"link disabled"}`. No click event. |

## 5. Backend design

### 5.1 Packages

- `internal/service/admin_service.go`: `AdminService` with the operations above.
  Depends on an `AdminRepository` interface (defined at the consumer, like the
  others), `LinkCache`, `TokenCache` (for `RevokeAllRefreshTokens`).
- `internal/repository/admin_mysql.go`: list/search queries, and every write as
  one transaction that also inserts the `audit_log` row.
- `internal/handler/admin.go`: routes, input parsing, JSON shapes.
- `internal/middleware/auth.go`: `RequireAdmin(lookup)`, where `lookup` returns
  role and disabled state for a user ID.
- `cmd/server`: an `admin grant|revoke <email>` subcommand, run with
  `docker compose run --rm server admin grant a@b.com`. Exits non-zero when the
  email is unknown.

### 5.2 Redirect

- The cache still stores only the URL, or the marker value `!disabled`.
- On cache miss, `GetByCode` also reads `links.disabled_at` and the owner's
  `users.disabled_at` (one JOIN). Either set → `ErrLinkDisabled`.
- A disabled result is cached as `!disabled` for 5 minutes, so clicks on a
  disabled phishing link do not each query MySQL. A cache hit on the marker
  returns `ErrLinkDisabled`.
- Disable link: update DB, then `DEL sl:<code>`. Enable: same.
- Disable / enable user: update DB, then delete the cache keys of all their
  links (codes read from MySQL, `DEL` in batches of 500).
- Known gap (same as today's delete): a cache miss that read the row just
  before the update can write the old URL back to the cache a few milliseconds
  after the `DEL`. It then lives until the cache TTL. Accepted; a second
  delayed `DEL` can be added if it is ever observed.

### 5.3 Disabling a user, in order

1. Transaction: set `users.disabled_at`, insert audit row.
2. `RevokeAllRefreshTokens(userID)`.
3. Delete cache keys of their links.

If step 2 or 3 fails, the request returns `500` and the admin retries; every
step is idempotent. The access token keeps working for up to 15 minutes, but
link creation and admin access check the database.

### 5.4 Analytics (worker)

- `BatchUpdateClicks` sorts codes before updating (fixes NFR-R8), and in the
  same transaction upserts per-day rows:

  ```sql
  INSERT INTO analytics_daily (link_id, stat_date, clicks)
  SELECT id, UTC_DATE(), ? FROM links WHERE short_code = ?
  ON DUPLICATE KEY UPDATE clicks = clicks + VALUES(clicks)
  ```

- The day is when the worker writes the batch (UTC). A click a second before
  midnight can land on the next day. Accepted.
- Clicks recorded before this ships are not split by day; totals still include
  them through `links.clicks`.

## 6. GUI design (`shorten-link-gui`)

### 6.1 Routes

| Route | Content |
|---|---|
| `/[locale]/admin` | Dashboard: total tiles, line chart of the daily series (7/30/90 days), top links table |
| `/[locale]/admin/users` | Searchable, filterable table; row opens detail |
| `/[locale]/admin/users/[id]` | Detail, quota edit, disable/enable, the user's links (links list filtered by `user_id`) |
| `/[locale]/admin/links` | Searchable, filterable table with disable/enable/delete actions |
| `/[locale]/admin/audit` | Audit log table |

`src/app/[locale]/admin/layout.tsx` renders a sidebar and wraps children in
`AdminGate`. Pages stay thin, per the GUI's CLAUDE.md.

### 6.2 Code layout (`src/features/admin/`)

- `model/`: API functions over `authed-http`, Zod schemas for every response
  (IDs as strings), error mapping.
- `view-models/`: one hook per screen (TanStack Query for lists with cursor
  paging, mutations that invalidate the affected queries).
- `components/`: `AdminGate`, `AdminSidebar`, tables, `ConfirmActionDialog`
  (alert-dialog with an optional reason field), stat tiles, chart.

`AdminGate` calls `/api/admin/me`: not signed in → redirect to login; `403` →
the existing `permission-state`. The account menu shows an "Admin" link when the
same query returns 200.

New shadcn components via CLI: `table`, `chart`. `chart` adds the dependency
**recharts**, the only new dependency.

Every string goes in `messages/en.json` (and any other locale files, checked by
`pnpm i18n:check`).

### 6.3 Behaviour

- Disable, enable and delete always ask for confirmation; disable/delete offer
  a reason field. Result shown with a toast; the row updates from the mutation
  response.
- Search inputs are debounced (300 ms) and kept in the URL (`?q=&status=`) so
  a view can be shared and survives reload.
- Empty, loading and error states use the existing common components.

## 7. Testing

**API**

- Unit: `AdminService` (self-disable refused, idempotent disable, quota range,
  order of disable steps); `RequireAdmin` (401/403/pass, disabled admin);
  handlers (status codes, string IDs, cursor); `Resolve` returns
  `ErrLinkDisabled` for a disabled link, a disabled owner and the cached marker;
  login and refresh for a disabled user; `Shorten` for a disabled user.
- Integration (MySQL in Docker): list/search/filters and paging order; each
  write leaves exactly one audit row, and none when the transaction fails;
  `BatchUpdateClicks` writes `analytics_daily` and adds on a second batch;
  stats zero-fills missing days.
- CLI: grant/revoke on a known and an unknown email.

**GUI**

- Vitest: Zod schemas (string IDs, nullable fields), view-models (paging,
  invalidation after a mutation), `AdminGate` states.
- Playwright: an admin disables a link and it moves from the active filter to
  the disabled one (API mocked at the network layer, as existing e2e tests do).
- `pnpm verify` passes.

## 8. Delivery

Three plans, in order; each is its own branch and PR.

1. **API core:** migration 006, DSN time zone, `RequireAdmin`, CLI, users,
   links, audit, `me`, the changes in §4.7, redirect behaviour in §5.2.
2. **API analytics:** worker upsert + ordering, `/api/admin/stats`.
3. **GUI:** everything in §6. Can start once plan 1's API is merged; the
   dashboard needs plan 2.

## 9. Documentation (same commits as the code)

- API: `api/openapi.yaml` (admin routes, 410 on redirect, 403 on login/create);
  `docs/architect.md` routes table and chain; `docs/spec.md` (FR-L10 partial,
  FR-S2, NFR-S12, NFR-R8, new FR-ADM rows); `docs/modules/` pages for
  handler, service, repository, middleware, server (CLI), worker, database
  (migration 006); `docs/deploy.md` (granting the first admin).
- GUI: `README.md` routes; feature README if the repo keeps one per feature.
