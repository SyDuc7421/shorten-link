# Admin Stats Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record clicks per link per UTC day and give admins `GET /api/admin/stats` with totals, a per-day series and top links.

**Architecture:** The worker's existing click batch transaction also upserts `analytics_daily` rows (and updates codes in sorted order). A read-only `Stats` query on `AdminRepository` returns totals plus sparse per-day rows; `AdminService.Stats` validates the range and zero-fills missing days; `AdminHandler` serves it behind the existing admin guard.

**Tech Stack:** Go 1.26, sqlx + MySQL 8, testcontainers.

**Spec:** `docs/superpowers/specs/2026-10-18-admin-design.md` §4.6 and §5.4. Builds on plan 1 (`docs/superpowers/plans/2026-10-18-admin-api-core.md`), branch `feat/admin-api`.

## Global Constraints

- `days` query parameter: integer `1..90`, default 30; anything else → `400`.
- Day boundaries are UTC. The MySQL session is already UTC (plan 1 DSN).
- `daily` has exactly `days` entries, oldest first, ending today (UTC), zeros filled in; `date` formatted `YYYY-MM-DD`.
- `top_links`: top 10 by clicks within the range, from `analytics_daily`.
- `analytics_daily` is written in the same transaction as `links.clicks`, so both move together or not at all.
- Docs ship in the same commit as the code.

## Review Focus

1. **A click event for a code that no longer exists** (link deleted between click and batch) → batch still commits, no `analytics_daily` row, no error. Test in Task 1.
2. **Two batches for the same link on the same day** → one row, clicks summed. Test in Task 1.
3. **`days=0`, `days=91`, `days=abc`** → 400. Test in Task 3.
4. **A day with no activity in the middle of the range** → present with zeros. Test in Task 2.
5. **Clicks on a link deleted later** → still counted in `daily.clicks`, absent from `top_links` (JOIN). Test in Task 2.

---

### Task 1: Worker writes `analytics_daily`

**Files:**
- Modify: `internal/repository/link_mysql.go` (`BatchUpdateClicks`)
- Test: `internal/repository/integration_test.go`
- Docs: `docs/modules/repository.md`, `docs/modules/messaging.md` and/or `docs/modules/worker.md`, `docs/modules/database.md` (`analytics_daily` now written)

**Interfaces:** unchanged (`BatchUpdateClicks(ctx, counts map[string]int64) error`).

- [ ] **Step 1: Failing test** `TestLinkMySQL_BatchUpdateClicks_DailyRows`: two links; batch 1 `{a:3, b:1, gone:5}`, batch 2 `{a:2}`; assert `links.clicks` a=5 b=1; `analytics_daily` has one row for a with clicks 5 and `stat_date = UTC_DATE()`, one row for b with 1, none for `gone`; no error.
- [ ] **Step 2: Run** → FAIL (no rows).
- [ ] **Step 3: Implement.** Inside the existing transaction, iterate codes in sorted order (`slices.Sorted(maps.Keys(counts))`) and for each run:

```sql
UPDATE links SET clicks = clicks + ? WHERE short_code = ?;
INSERT INTO analytics_daily (link_id, stat_date, clicks)
SELECT id, UTC_DATE(), ? FROM links WHERE short_code = ?
ON DUPLICATE KEY UPDATE clicks = clicks + VALUES(clicks);
```
Sorted order means two workers lock rows in the same order and cannot deadlock (NFR-R8).
- [ ] **Step 4: Run** → PASS; existing `TestLinkMySQL_BatchUpdateClicks` and `./internal/messaging` tests still pass.
- [ ] **Step 5: Docs + commit** `feat(worker): record clicks per link per day`.

---

### Task 2: Stats query and service

**Files:**
- Modify: `internal/service/admin.go` (types, `AdminRepository.Stats`, `ErrInvalidDays`), `internal/service/admin_service.go` (`Stats`), `internal/repository/admin_mysql.go`
- Test: `internal/service/admin_service_test.go`, `internal/repository/admin_integration_test.go`
- Docs: `docs/modules/service.md`, `docs/modules/repository.md`

**Interfaces (Produces):**

```go
var ErrInvalidDays = errors.New("service: days must be 1-90")

const (
	MaxStatsDays     = 90
	DefaultStatsDays = 30
	TopLinksLimit    = 10
)

type StatsTotals struct {
	Users, Links, Clicks, DisabledUsers, DisabledLinks int64
}

type DayStats struct {
	Date                       time.Time // UTC midnight
	NewUsers, NewLinks, Clicks int64
}

type TopLink struct {
	Code   string
	Clicks int64
}

type Stats struct {
	Totals   StatsTotals
	Daily    []DayStats // repository: only days with activity; service: every day
	TopLinks []TopLink
}

// in AdminRepository:
Stats(ctx context.Context, since time.Time) (*Stats, error) // since = UTC midnight of the first day

func (s *AdminService) Stats(ctx context.Context, days int) (*Stats, error)
```

- [ ] **Step 1: Failing tests.**
  - Service `TestStats_DaysRange`: 0 and 91 → `ErrInvalidDays`; 1 and 90 OK.
  - Service `TestStats_ZeroFillsAndOrders`: fake repo returns rows for today and today-2 only; `Stats(ctx, 3)` → 3 entries oldest first, the middle one zero; `since` passed to repo = UTC midnight two days ago.
  - Integration `TestAdminMySQL_Stats`: users (one disabled), links (one disabled), `analytics_daily` rows for today and yesterday, one for a deleted link ID; assert totals, per-day `NewUsers`/`NewLinks`/`Clicks` (deleted link's clicks included), `TopLinks` ordered desc and without the deleted link, rows before `since` excluded.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** `Stats` in `admin_mysql.go`:

```sql
-- totals
SELECT (SELECT COUNT(*) FROM users) AS users,
       (SELECT COUNT(*) FROM links) AS links,
       (SELECT COALESCE(SUM(clicks), 0) FROM links) AS clicks,
       (SELECT COUNT(*) FROM users WHERE disabled_at IS NOT NULL) AS disabled_users,
       (SELECT COUNT(*) FROM links WHERE disabled_at IS NOT NULL) AS disabled_links;
-- per day (three queries, merged by date in Go)
SELECT DATE(created_at) AS d, COUNT(*) AS n FROM users WHERE created_at >= ? GROUP BY d;
SELECT DATE(created_at) AS d, COUNT(*) AS n FROM links WHERE created_at >= ? GROUP BY d;
SELECT stat_date AS d, SUM(clicks) AS n FROM analytics_daily WHERE stat_date >= ? GROUP BY stat_date;
-- top links
SELECT l.short_code AS code, SUM(a.clicks) AS clicks
FROM analytics_daily a JOIN links l ON l.id = a.link_id
WHERE a.stat_date >= ?
GROUP BY a.link_id, l.short_code
ORDER BY clicks DESC, a.link_id DESC
LIMIT 10;
```
Service: validate, `since := today.AddDate(0,0,-(days-1))`, build a map by `Date`, emit `days` entries.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Docs + commit** `feat(admin): add stats query and service`.

---

### Task 3: `GET /api/admin/stats`

**Files:**
- Modify: `internal/handler/admin.go` (route, `AdminServicer.Stats`), `internal/handler/admin_handler_test.go`
- Docs: `api/openapi.yaml`, `docs/architect.md` routes, `docs/modules/handler.md`, `docs/spec.md` (FR-S2 ✅, FR-ADM8 ✅, NFR-R8 ✅, FR-S3 ⚠️ admin-wide only)

**Interfaces:** Consumes `AdminService.Stats(ctx, days int) (*service.Stats, error)`.

JSON:
```json
{
  "totals": {"users": 0, "links": 0, "clicks": 0, "disabled_users": 0, "disabled_links": 0},
  "daily": [{"date": "2026-10-18", "new_users": 0, "new_links": 0, "clicks": 0}],
  "top_links": [{"code": "abc", "clicks": 0}]
}
```
`top_links` and `daily` are `[]`, never `null`.

- [ ] **Step 1: Failing tests:** default `days` → service got 30; `?days=7` → 7; `days=0|91|abc` → 400 (service not called... mock returns `ErrInvalidDays` for range; `abc` rejected by handler); JSON shape with `date` as `YYYY-MM-DD`; route included in `TestAdminHandler_RoutesRequireAdmin`.
- [ ] **Step 2–4:** implement, run `go test ./...`, lint, Redocly lint.
- [ ] **Step 5: Docs + commit** `feat(admin): add GET /api/admin/stats`.
