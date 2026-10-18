# Admin API Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give admins an API to list, search, disable and edit users and links, with every change audited, and make disabled users and links actually stop working.

**Architecture:** A new `AdminService` (service layer) over a new `AdminRepository` (MySQL, one transaction per write including its `audit_log` row), exposed by `AdminHandler` under `/api/admin/*` behind `RequireAuth` → `RequireAdmin`. Disabled state lives in `users.disabled_at` / `links.disabled_at`; redirect reads it on cache miss and caches a `!disabled` marker for 5 minutes. The first admin is granted by a `server admin grant <email>` subcommand.

**Tech Stack:** Go 1.26, net/http ServeMux patterns, sqlx + MySQL 8, go-redis v9, zap, testcontainers for integration tests.

**Spec:** `docs/superpowers/specs/2026-10-18-admin-design.md` (§3, §4.1–4.5, §4.7, §5.1–5.3). Stats (§4.6, §5.4) and GUI (§6) are plans 2 and 3.

## Global Constraints

- All IDs in admin JSON are strings; times RFC 3339 UTC.
- Admin lists: newest first, keyset cursor on `id` (`id < cursor`), 20 per page, `next_cursor` empty on the last page.
- `quota_daily` accepted range: `0..100000`. `reason`: optional, max 500 bytes.
- Disabled link marker in cache: value `!disabled`, TTL 5 minutes.
- Cache key deletion for a user's links: `DEL` in batches of 500.
- Errors keep the `{"error":"..."}` shape. Not admin / disabled admin: `403 {"error":"forbidden"}`.
- Login for a disabled user: `403 {"error":"account disabled"}`, only after the password matched.
- Every admin write and its `audit_log` row are one transaction; a no-op (already in that state) writes no audit row.
- CLI audit rows use `user_id = 0`.
- Docs ship in the same commit as the code (project CLAUDE.md §5).

## Review Focus

1. **Admin disables themselves** → `409 cannot disable yourself`, nothing changes. Test in Task 5.
2. **Disable retried after a partial failure** (DB updated, token revoke failed) → second call still revokes and purges cache although the row is already disabled. Test in Task 5.
3. **Cursor or ID that is not a number** (`?cursor=abc`, `/users/xyz`) → `400`, not `500`. Test in Task 7.
4. **Enabling a user whose links were cached as `!disabled`** → links redirect again immediately (markers purged). Test in Task 5.
5. **A link individually disabled stays disabled after its owner is disabled and re-enabled.** Test in Task 4 (`status=active` filter) and Task 3 (`Resolve`).

---

### Task 1: Schema and domain fields

**Files:**
- Create: `deploy/migrations/006_admin.up.sql`, `deploy/migrations/006_admin.down.sql`
- Modify: `config/connect.go` (DSN), `internal/service/auth.go` (`User`), `internal/service/link.go` (`Link`), `internal/repository/user_mysql.go`, `internal/repository/integration_test.go` (run 006)
- Docs: `docs/modules/database.md`, `docs/modules/config.md` (DSN note)

**Interfaces:**
- Produces: `service.User{Role string; DisabledAt *time.Time}`, consts `service.RoleUser = "user"`, `service.RoleAdmin = "admin"`; `service.Link{DisabledAt *time.Time; OwnerDisabled bool}`.

- [ ] **Step 1: Failing integration test** in `integration_test.go`:

```go
func TestUserMySQL_GetByID_RoleAndDisabled(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewUserMySQL(db)
	ctx := context.Background()
	if err := repo.Create(ctx, &service.User{ID: 1, Email: "a@b.com", PasswordHash: "h", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	u, err := repo.GetByID(ctx, 1)
	if err != nil || u.Role != service.RoleUser || u.DisabledAt != nil {
		t.Fatalf("want role user, not disabled; got %+v, %v", u, err)
	}
	db.ExecContext(ctx, `UPDATE users SET role='admin', disabled_at=UTC_TIMESTAMP() WHERE id=1`) //nolint:errcheck
	u, _ = repo.GetByEmail(ctx, "a@b.com")
	if u.Role != service.RoleAdmin || u.DisabledAt == nil {
		t.Fatalf("want admin + disabled, got %+v", u)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/repository/ -run RoleAndDisabled` → FAIL (`u.Role undefined`).

- [ ] **Step 3: Implement.**

`006_admin.up.sql`:
```sql
ALTER TABLE users
  ADD COLUMN role        VARCHAR(16) NOT NULL DEFAULT 'user' AFTER plan,
  ADD COLUMN disabled_at DATETIME    NULL;
ALTER TABLE links
  ADD COLUMN disabled_at DATETIME NULL;
ALTER TABLE audit_log
  ADD KEY idx_created (created_at);
```
`006_admin.down.sql`:
```sql
ALTER TABLE audit_log DROP KEY idx_created;
ALTER TABLE links DROP COLUMN disabled_at;
ALTER TABLE users DROP COLUMN disabled_at, DROP COLUMN role;
```
Add `"006_admin.up.sql"` to the migration list in `setupMySQL`.
`config/connect.go` DSN: `"%s:%s@tcp(%s:%d)/%s?parseTime=true&time_zone=%%27%%2B00%%3A00%%27"` with a comment: session time zone UTC so `DATE()`/`CURDATE()` agree with the UTC day used everywhere.
`User` gains `Role string`, `DisabledAt *time.Time`; `Link` gains `DisabledAt *time.Time`, `OwnerDisabled bool`. `user_mysql.go` `GetByEmail`/`GetByID` select `role, disabled_at` and map them.

- [ ] **Step 4: Run** the test → PASS; `go test ./...` → all pass.
- [ ] **Step 5: Docs + commit** `feat(db): add user role and disabled flags for admin`.

---

### Task 2: Disabled users cannot sign in, refresh or create links

**Files:**
- Modify: `internal/service/auth.go` (error), `internal/service/auth_service.go` (`Login`, `Refresh`), `internal/handler/auth.go` (login 403), `internal/service/link.go` (`Quota`, `LinkRepository.QuotaUsage`), `internal/service/link_service.go`, `internal/handler/link.go` (403), `internal/repository/link_mysql.go` (`QuotaUsage`)
- Test: `internal/service/auth_service_test.go`, `internal/service/link_service_test.go`, `internal/handler/auth_handler_test.go`, `internal/handler/link_handler_test.go`, `internal/repository/integration_test.go`
- Docs: `docs/modules/service.md`, `docs/modules/handler.md`, `api/openapi.yaml` (403 on login and create)

**Interfaces:**
- Produces: `service.ErrAccountDisabled`; `type Quota struct{ Used, Limit int; Disabled bool }`; `LinkRepository.QuotaUsage(ctx, userID, sinceID int64) (Quota, error)`.

- [ ] **Step 1: Failing tests.**
  - `TestLogin_DisabledUserCorrectPassword` → `ErrAccountDisabled`.
  - `TestLogin_DisabledUserWrongPassword` → `ErrInvalidCredentials` (status not revealed).
  - `TestRefresh_DisabledUser` → `ErrInvalidToken`.
  - `TestShorten_DisabledUser` → `ErrAccountDisabled`, `Create` not called (mock `quotaFn` returns `Quota{Limit:100, Disabled:true}`).
  - Handler: login `ErrAccountDisabled` → 403 `account disabled`; shorten `ErrAccountDisabled` → 403.
  - Integration: `TestLinkMySQL_QuotaUsage` updated to `Quota` and asserts `Disabled` after `UPDATE users SET disabled_at=...`.
- [ ] **Step 2: Run** → compile failures / FAIL.
- [ ] **Step 3: Implement.**
  - `Login`: after successful bcrypt and `DeleteLockout`, `if user.DisabledAt != nil { return nil, ErrAccountDisabled }`.
  - `Refresh`: after consume, `u, err := s.users.GetByID(ctx, userID)`; `ErrNotFound` or `u.DisabledAt != nil` → `ErrInvalidToken`; other errors wrapped.
  - `QuotaUsage` SQL selects `u.disabled_at IS NOT NULL AS disabled` too. `Shorten`: `if q.Disabled { return nil, ErrAccountDisabled }` before the quota comparison.
- [ ] **Step 4: Run** `go test ./...` → PASS.
- [ ] **Step 5: Docs + commit** `feat(auth): refuse login, refresh and link creation for disabled users`.

---

### Task 3: Redirect refuses disabled links and owners

**Files:**
- Modify: `internal/service/link.go` (`ErrLinkDisabled`, `LinkCache.DeleteMany`), `internal/service/link_service.go` (`Resolve`), `internal/repository/link_mysql.go` (`GetByCode` JOIN), `internal/repository/link_redis.go` (`DeleteMany`), `internal/handler/link.go` (410)
- Test: `internal/service/link_service_test.go`, `internal/handler/link_handler_test.go`, `internal/repository/integration_test.go`
- Docs: `docs/modules/service.md` (Resolve), `docs/modules/repository.md`, `api/openapi.yaml` (410 on `GET /{code}`), `docs/architect.md` (routes)

**Interfaces:**
- Produces: `service.ErrLinkDisabled`; `LinkCache.DeleteMany(ctx, codes []string) error` (batches of 500); `const disabledMarker = "!disabled"`, `disabledCacheTTL = 5 * time.Minute` (service package, unexported).

- [ ] **Step 1: Failing tests.**
  - `TestResolve_DisabledLink`: repo returns `Link{DisabledAt:&t}` → `ErrLinkDisabled`, cache now holds `!disabled`, no publish.
  - `TestResolve_DisabledOwner`: `Link{OwnerDisabled:true}` → same.
  - `TestResolve_CachedDisabledMarker`: cache has `!disabled` → `ErrLinkDisabled`, repo not called.
  - Handler: `ErrLinkDisabled` → 410 `link disabled`.
  - Integration: `GetByCode` returns `DisabledAt` and `OwnerDisabled`; `LinkRedis.DeleteMany` with 1200 codes deletes all.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**

`GetByCode` SQL:
```sql
SELECT l.id, l.user_id, l.short_code, l.original_url, l.clicks, l.expires_at,
       l.disabled_at, u.disabled_at IS NOT NULL AS owner_disabled, l.created_at, l.updated_at
FROM links l LEFT JOIN users u ON u.id = l.user_id
WHERE l.short_code = ?
```
`Resolve`: on cache hit, `if rawURL == disabledMarker { return "", ErrLinkDisabled }`. After DB read: `if link.DisabledAt != nil || link.OwnerDisabled { s.cache.Set(ctx, code, disabledMarker, disabledCacheTTL); return "", ErrLinkDisabled }` (before the expiry check). `DeleteMany`: loop chunks of 500, `rdb.Del(ctx, keys...)`. `mockCache` in tests gains `DeleteMany`.
- [ ] **Step 4: Run** `go test ./...` → PASS.
- [ ] **Step 5: Docs + commit** `feat(links): answer 410 for disabled links and links of disabled users`.

---

### Task 4: Admin repository

**Files:**
- Create: `internal/service/admin.go` (types + `AdminRepository`), `internal/repository/admin_mysql.go`
- Test: `internal/repository/admin_integration_test.go` (same package `repository_test`, reuses `setupMySQL`)
- Docs: `docs/modules/repository.md` (admin section)

**Interfaces (Produces, in `internal/service/admin.go`):**

```go
var (
	ErrSelfDisable  = errors.New("service: cannot disable yourself")
	ErrInvalidQuota = errors.New("service: quota_daily must be 0-100000")
	ErrReasonTooLong = errors.New("service: reason must be at most 500 bytes")
)

const (
	MaxQuotaDaily  = 100000
	MaxReasonBytes = 500
	AdminPageSize  = 20
)

type AdminUser struct {
	ID          int64
	Email       string
	Role        string
	QuotaDaily  int
	DisabledAt  *time.Time
	CreatedAt   time.Time
	LinkCount   int64
	LinksToday  int   // only filled by GetUser
	TotalClicks int64 // only filled by GetUser
}

type AdminLink struct {
	ID              int64
	ShortCode       string
	OriginalURL     string
	Clicks          int64
	DisabledAt      *time.Time
	ExpiresAt       *time.Time
	CreatedAt       time.Time
	OwnerID         int64
	OwnerEmail      string
	OwnerDisabledAt *time.Time
}

type AuditEntry struct {
	ID           int64
	ActorID      int64  // 0 = system
	ActorEmail   string // "" for system
	Action       string
	ResourceType string
	ResourceID   int64
	Metadata     json.RawMessage
	CreatedAt    time.Time
}

type UserFilter struct {
	Query    string // email contains
	Status   string // "", "active", "disabled"
	BeforeID int64  // 0 = first page
}

type LinkFilter struct {
	Query    string // code equals or URL contains
	Status   string // "", "active", "disabled", "expired"
	UserID   int64  // 0 = any
	BeforeID int64
}

type AuditFilter struct {
	ActorID      int64  // 0 = any
	ResourceType string // "", "user", "link"
	BeforeID     int64
}

// AdminRepository is the persistence interface for admin operations. Every
// write runs in one transaction with its audit_log row; changed=false means the
// row was already in that state and nothing (no audit) was written.
type AdminRepository interface {
	ListUsers(ctx context.Context, f UserFilter) ([]*AdminUser, error)
	GetUser(ctx context.Context, id, todaySinceID int64) (*AdminUser, error)
	SetUserQuota(ctx context.Context, actorID, userID int64, quota int, reason string) error
	SetUserDisabled(ctx context.Context, actorID, userID int64, disabled bool, reason string) (changed bool, err error)
	LinkCodesByUser(ctx context.Context, userID int64) ([]string, error)
	ListLinks(ctx context.Context, f LinkFilter) ([]*AdminLink, error)
	GetLink(ctx context.Context, code string) (*AdminLink, error)
	SetLinkDisabled(ctx context.Context, actorID int64, code string, disabled bool, reason string) (changed bool, err error)
	DeleteLink(ctx context.Context, actorID int64, code string, reason string) error
	ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error)
	SetRole(ctx context.Context, actorID int64, email, role string) (changed bool, err error)
}
```

Audit actions: `user.quota` (metadata `before`, `after`, `reason`?), `user.disable`, `user.enable`, `link.disable`, `link.enable`, `link.delete` (metadata `code`, `reason`?; links also carry `code`), `admin.grant`, `admin.revoke`. `resource_type` `user` / `link`. Unknown user/link/email → `ErrNotFound`.

- [ ] **Step 1: Failing integration tests** (`admin_integration_test.go`), seed helper inserting users 1..3 and links with known IDs:
  - `TestAdminMySQL_ListUsers_NewestFirstAndPaging`: 25 users → first page 20 by id desc, second page (BeforeID = last id) 5.
  - `TestAdminMySQL_ListUsers_FilterQueryAndStatus`: `Query:"b.c"` matches only those emails; `Status:"disabled"` only disabled; `LinkCount` correct.
  - `TestAdminMySQL_GetUser_TodayAndClicks`: `LinksToday` counts `id >= todaySinceID`, `TotalClicks` sums clicks.
  - `TestAdminMySQL_SetUserDisabled_AuditOnceAndIdempotent`: first call changed=true and one `user.disable` audit row with `reason`; second call changed=false, still one row; enable → `user.enable` row.
  - `TestAdminMySQL_SetUserQuota_AuditBeforeAfter`: metadata `{"before":100,"after":5}`.
  - `TestAdminMySQL_ListLinks_Filters`: `Status:"active"` excludes disabled link, link of disabled owner and expired link; `"disabled"` includes individually disabled and disabled-owner links; `"expired"` only expired; `Query` exact code and URL substring; `UserID` filter; owner email present.
  - `TestAdminMySQL_SetLinkDisabled_And_DeleteLink`: audit rows have `resource_id` = link ID and `metadata.code`; delete removes row and writes `link.delete`; delete unknown → `ErrNotFound`, no audit row.
  - `TestAdminMySQL_ListAudit_ActorEmailAndFilters`: actor email joined; actor 0 → `ActorEmail == ""`; `ResourceType` filter.
  - `TestAdminMySQL_SetRole`: grant by email → role admin, audit `admin.grant` with `user_id=0`; unknown email → `ErrNotFound`.
  - `TestAdminMySQL_LinkCodesByUser`.
- [ ] **Step 2: Run** `go test ./internal/repository/ -run AdminMySQL` → compile FAIL.
- [ ] **Step 3: Implement `admin_mysql.go`.** Key SQL:

ListUsers (build `WHERE` from filter, args in order):
```sql
SELECT u.id, u.email, u.role, u.quota_daily, u.disabled_at, u.created_at,
       (SELECT COUNT(*) FROM links l WHERE l.user_id = u.id) AS link_count
FROM users u
WHERE 1=1 [AND u.id < ?] [AND u.email LIKE ?] [AND u.disabled_at IS NULL | IS NOT NULL]
ORDER BY u.id DESC LIMIT 20
```
`LIKE` argument: `"%" + escapeLike(q) + "%"` where `escapeLike` escapes `\`, `%`, `_`.

ListLinks:
```sql
SELECT l.id, l.short_code, l.original_url, l.clicks, l.disabled_at, l.expires_at, l.created_at,
       l.user_id, COALESCE(u.email, ''), u.disabled_at
FROM links l LEFT JOIN users u ON u.id = l.user_id
WHERE 1=1 [AND l.id < ?] [AND l.user_id = ?]
  [AND (l.short_code = ? OR l.original_url LIKE ?)]
  -- active:   l.disabled_at IS NULL AND u.disabled_at IS NULL AND (l.expires_at IS NULL OR l.expires_at > UTC_TIMESTAMP())
  -- disabled: (l.disabled_at IS NOT NULL OR u.disabled_at IS NOT NULL)
  -- expired:  l.expires_at IS NOT NULL AND l.expires_at <= UTC_TIMESTAMP()
ORDER BY l.id DESC LIMIT 20
```

Writes follow one pattern:
```go
func (r *adminMySQL) SetUserDisabled(ctx context.Context, actorID, userID int64, disabled bool, reason string) (bool, error) {
	var changed bool
	err := r.inTx(ctx, func(tx *sqlx.Tx) error {
		var cur *time.Time
		if err := tx.GetContext(ctx, &cur, `SELECT disabled_at FROM users WHERE id = ? FOR UPDATE`, userID); err != nil {
			return notFound(err)
		}
		if (cur != nil) == disabled {
			return nil
		}
		q := `UPDATE users SET disabled_at = NULL WHERE id = ?`
		action := "user.enable"
		if disabled {
			q = `UPDATE users SET disabled_at = UTC_TIMESTAMP() WHERE id = ?`
			action = "user.disable"
		}
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return err
		}
		changed = true
		return insertAudit(ctx, tx, actorID, action, "user", userID, meta(reason, nil))
	})
	return changed, err
}
```
`inTx` begins, runs fn, commits or rolls back. `notFound` maps `sql.ErrNoRows` → `service.ErrNotFound`. `meta(reason, extra map[string]any)` returns JSON with `reason` only when non-empty. `insertAudit` does `INSERT INTO audit_log (user_id, action, resource_type, resource_id, metadata) VALUES (?,?,?,?,?)`.

ListAudit:
```sql
SELECT a.id, a.user_id, COALESCE(u.email, ''), a.action, a.resource_type, COALESCE(a.resource_id, 0),
       COALESCE(a.metadata, JSON_OBJECT()), a.created_at
FROM audit_log a LEFT JOIN users u ON u.id = a.user_id
WHERE 1=1 [AND a.id < ?] [AND a.user_id = ?] [AND a.resource_type = ?]
ORDER BY a.id DESC LIMIT 20
```
- [ ] **Step 4: Run** → PASS; `golangci-lint run ./...` → 0 issues.
- [ ] **Step 5: Docs + commit** `feat(admin): add admin repository with audited writes`.

---

### Task 5: Admin service

**Files:**
- Create: `internal/service/admin_service.go`, `internal/service/admin_service_test.go`
- Docs: `docs/modules/service.md` (AdminService section)

**Interfaces:**
- Consumes: `AdminRepository` (Task 4), `UserRepository`, `LinkCache` incl. `DeleteMany` (Task 3), `TokenCache.RevokeAllRefreshTokens`.
- Produces:

```go
func NewAdminService(repo AdminRepository, users UserRepository, cache LinkCache, tokens TokenCache) *AdminService
func (s *AdminService) Me(ctx context.Context, userID int64) (*User, error)
func (s *AdminService) IsActiveAdmin(ctx context.Context, userID int64) (bool, error)
func (s *AdminService) ListUsers(ctx context.Context, f UserFilter) ([]*AdminUser, error)
func (s *AdminService) GetUser(ctx context.Context, id int64) (*AdminUser, error)
func (s *AdminService) SetQuota(ctx context.Context, actorID, userID int64, quota int, reason string) (*AdminUser, error)
func (s *AdminService) DisableUser(ctx context.Context, actorID, userID int64, reason string) (*AdminUser, error)
func (s *AdminService) EnableUser(ctx context.Context, actorID, userID int64, reason string) (*AdminUser, error)
func (s *AdminService) ListLinks(ctx context.Context, f LinkFilter) ([]*AdminLink, error)
func (s *AdminService) DisableLink(ctx context.Context, actorID int64, code, reason string) (*AdminLink, error)
func (s *AdminService) EnableLink(ctx context.Context, actorID int64, code, reason string) (*AdminLink, error)
func (s *AdminService) DeleteLink(ctx context.Context, actorID int64, code, reason string) error
func (s *AdminService) ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error)
func (s *AdminService) SetAdmin(ctx context.Context, email string, admin bool) (changed bool, err error) // CLI, actor 0
```

Rules: `reason` > 500 bytes → `ErrReasonTooLong`; quota outside 0..100000 → `ErrInvalidQuota`; `DisableUser` with `actorID == userID` → `ErrSelfDisable`. `DisableUser` order: repo `SetUserDisabled(true)` → `RevokeAllRefreshTokens(userID, now, refreshTokenTTL)` → `DeleteMany(LinkCodesByUser)`; steps 2–3 run even when `changed == false` so a retry finishes a half-done disable. `EnableUser`: repo, then `DeleteMany` (clears `!disabled` markers). Link disable/enable/delete: repo, then `cache.Delete(code)`. `GetUser` passes `idgen.MinIDAt(UTC midnight)`. `IsActiveAdmin`: `GetByID`; `ErrNotFound` → false, nil.

- [ ] **Step 1: Failing unit tests** with in-file fakes (`fakeAdminRepo` recording calls, reuse `mockCache` + a `fakeTokens` recording `RevokeAllRefreshTokens`):
  - `TestDisableUser_Self` → `ErrSelfDisable`, repo not called.
  - `TestDisableUser_RevokesAndPurges`: revoke called for user, `DeleteMany` got the user's codes.
  - `TestDisableUser_RetryWhenAlreadyDisabled`: repo returns changed=false → revoke and purge still run.
  - `TestDisableUser_RevokeFails` → error returned (handler → 500).
  - `TestEnableUser_PurgesMarkers`: cache had `!disabled` for user's codes → gone.
  - `TestSetQuota_Range`: -1 and 100001 → `ErrInvalidQuota`; 0 and 100000 OK.
  - `TestReasonTooLong` (501 bytes) on DisableLink → `ErrReasonTooLong`.
  - `TestDisableLink_DeletesCache`, `TestDeleteLink_DeletesCache`.
  - `TestIsActiveAdmin`: admin → true; user → false; disabled admin → false; unknown → false, nil.
- [ ] **Step 2: Run** → FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/service/` → PASS.
- [ ] **Step 5: Docs + commit** `feat(admin): add admin service`.

---

### Task 6: `RequireAdmin` middleware

**Files:**
- Modify: `internal/middleware/auth.go`; Test: `internal/middleware/auth_test.go`
- Docs: `docs/modules/middleware.md`

**Interfaces:**
- Produces: `type AdminChecker interface { IsActiveAdmin(ctx context.Context, userID int64) (bool, error) }`; `func RequireAdmin(c AdminChecker) func(http.Handler) http.Handler`.

- [ ] **Step 1: Failing tests:** no user in context → 401; checker false → 403 `{"error":"forbidden"}` with `Content-Type: application/json`; checker error → 500; true → next called.
- [ ] **Step 2–4:** implement with `writeJSONError(w, status, msg)` helper inside middleware (sets header, `json.NewEncoder`); run → PASS.
- [ ] **Step 5: Docs + commit** `feat(middleware): add RequireAdmin`.

---

### Task 7: Admin HTTP handler and wiring

**Files:**
- Create: `internal/handler/admin.go`, `internal/handler/admin_handler_test.go`
- Modify: `cmd/server/main.go` (construct repo/service/handler, `Register`)
- Docs: `api/openapi.yaml` (all `/api/admin/*` + `AdminUser`, `AdminLink`, `AuditEntry` schemas), `docs/architect.md` (routes table), `docs/modules/handler.md`, `docs/modules/server.md`

**Interfaces:**
- Consumes: Task 5 methods via `type AdminServicer interface { ... }` (same signatures).
- Produces: `func NewAdminHandler(svc AdminServicer, baseURL string) *AdminHandler`; `func (h *AdminHandler) Register(mux *http.ServeMux, requireAuth, requireAdmin func(http.Handler) http.Handler)`.

Routes:
```
GET    /api/admin/me
GET    /api/admin/users
GET    /api/admin/users/{id}
PATCH  /api/admin/users/{id}
POST   /api/admin/users/{id}/disable
POST   /api/admin/users/{id}/enable
GET    /api/admin/links
POST   /api/admin/links/{code}/disable
POST   /api/admin/links/{code}/enable
DELETE /api/admin/links/{code}
GET    /api/admin/audit
```
Error mapping: `ErrNotFound` → 404 (`user not found` / `link not found`); `ErrSelfDisable` → 409; `ErrInvalidQuota`, `ErrReasonTooLong` → 400 with their message; bad `id`/`cursor`/`user_id`/`actor_id`/`status`/`resource_type` → 400; body too large → 413; else 500. JSON uses `strconv.FormatInt` for every ID; `next_cursor` = last ID when `len == AdminPageSize`. Optional body (`{"reason"}`) on POST/DELETE: empty body allowed.

- [ ] **Step 1: Failing tests** (mock `AdminServicer`):
  - `TestAdminHandler_ListUsers_StringIDsAndCursor`: response `id` is a JSON string; 20 rows → `next_cursor` = last id; passes `q`, `status`, `cursor` into filter.
  - `TestAdminHandler_BadParams`: `/users/xyz`, `?cursor=abc`, `?status=weird`, `/links?user_id=x` → 400.
  - `TestAdminHandler_DisableSelf` → 409.
  - `TestAdminHandler_PatchQuota`: missing `quota_daily` → 400; `ErrInvalidQuota` → 400; OK → 200 user JSON.
  - `TestAdminHandler_DeleteLink` → 204; not found → 404.
  - `TestAdminHandler_DisableLink_EmptyBody` → 200.
  - `TestAdminHandler_Audit_SystemActorNull`: `ActorID 0` → `"actor": null`.
  - `TestAdminHandler_RoutesRequireAdmin`: registered with a `requireAdmin` that returns 403 → every route 403.
- [ ] **Step 2–4:** implement; wire in `main.go`:

```go
adminRepo := repository.NewAdminMySQL(db)
adminSvc := service.NewAdminService(adminRepo, userRepo, linkCache, tokenCache)
adminHandler := handler.NewAdminHandler(adminSvc, cfg.BaseURL)
adminHandler.Register(mux, middleware.RequireAuth, middleware.RequireAdmin(adminSvc))
```
Run `go test ./...`, `golangci-lint run ./...`, YAML parse of `api/openapi.yaml` → pass.
- [ ] **Step 5: Docs + commit** `feat(admin): add /api/admin endpoints`.

---

### Task 8: `server admin grant|revoke` CLI

**Files:**
- Create: `cmd/server/admin_cli.go`, `cmd/server/admin_cli_test.go`
- Modify: `cmd/server/main.go` (dispatch before starting HTTP), `Makefile` (`admin-grant`, `admin-revoke`)
- Docs: `docs/deploy.md` (granting the first admin), `docs/modules/server.md`, `README.md` (command list if it has one)

**Interfaces:**
- Produces: `func runAdminCommand(ctx context.Context, args []string, svc adminSetter, out io.Writer) int` with `type adminSetter interface { SetAdmin(ctx context.Context, email string, admin bool) (bool, error) }`.

Behaviour: `admin grant <email>` / `admin revoke <email>`; prints `granted admin to a@b.com`, `a@b.com is already an admin`, etc.; unknown email → `no user with email a@b.com`, exit 1; wrong usage → usage text, exit 2. In `main`: `if len(os.Args) > 1 && os.Args[1] == "admin"` → load config, open MySQL only, build `repository.NewAdminMySQL` + `service.NewAdminService(repo, userRepo, nil, nil)`, `os.Exit(runAdminCommand(...))`. (`SetAdmin` never touches cache/tokens.)

- [ ] **Step 1: Failing tests:** grant OK → 0 + message; already → 0; unknown → 1; `admin` alone / `admin foo x` → 2.
- [ ] **Step 2–4:** implement, run → PASS.
- [ ] **Step 5: Docs + commit** `feat(server): add admin grant/revoke command`.

---

### Task 9: Spec status and final verification

**Files:** `docs/spec.md` (FR-L10 ⚠️ admin disable done, no automatic check; NFR-S12 ⚠️ admin actions audited only; add FR-ADM rows), `docs/modules/README.md` if new pages.

- [ ] **Step 1:** `go test ./...`, `golangci-lint run ./...`, `go vet ./...` all green; quote results.
- [ ] **Step 2:** Manual smoke if Docker is available: `docker compose up -d`, migrate, register a user, `go run ./cmd/server admin grant <email>`, login, `GET /api/admin/me` → 200, disable a link → redirect 410.
- [ ] **Step 3: Commit** `docs: update spec status for admin API`.
