package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/april/shorten-link/internal/service"
	"github.com/april/shorten-link/pkg/idgen"
)

type adminMySQL struct {
	db *sqlx.DB
}

// NewAdminMySQL returns an AdminRepository backed by MySQL.
func NewAdminMySQL(db *sqlx.DB) service.AdminRepository {
	return &adminMySQL{db: db}
}

// filter collects WHERE conditions and their arguments in order.
type filter struct {
	conds []string
	args  []any
}

func (f *filter) add(cond string, args ...any) {
	f.conds = append(f.conds, cond)
	f.args = append(f.args, args...)
}

func (f *filter) where() string {
	if len(f.conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(f.conds, " AND ")
}

// containsPattern turns q into a LIKE pattern matching q anywhere, with LIKE's
// own wildcards in q escaped so they match literally.
func containsPattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrNotFound
	}
	return err
}

func (r *adminMySQL) inTx(ctx context.Context, fn func(tx *sqlx.Tx) error) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("repository: begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("repository: commit: %w", err)
	}
	return nil
}

// auditMeta builds the metadata JSON: extra fields plus reason when not empty.
// It returns nil (SQL NULL) when there is nothing to store.
func auditMeta(reason string, extra map[string]any) ([]byte, error) {
	m := map[string]any{}
	for k, v := range extra {
		m[k] = v
	}
	if reason != "" {
		m["reason"] = reason
	}
	if len(m) == 0 {
		return nil, nil
	}
	return json.Marshal(m)
}

func insertAudit(ctx context.Context, tx *sqlx.Tx, actorID int64, action, resourceType string, resourceID int64, reason string, extra map[string]any) error {
	meta, err := auditMeta(reason, extra)
	if err != nil {
		return fmt.Errorf("repository: audit metadata: %w", err)
	}
	const q = `INSERT INTO audit_log (user_id, action, resource_type, resource_id, metadata) VALUES (?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q, actorID, action, resourceType, resourceID, meta); err != nil {
		return fmt.Errorf("repository: insert audit: %w", err)
	}
	return nil
}

// --- users ---

type adminUserRow struct {
	ID          int64      `db:"id"`
	Email       string     `db:"email"`
	Role        string     `db:"role"`
	QuotaDaily  int        `db:"quota_daily"`
	DisabledAt  *time.Time `db:"disabled_at"`
	CreatedAt   time.Time  `db:"created_at"`
	LinkCount   int64      `db:"link_count"`
	LinksToday  int        `db:"links_today"`
	TotalClicks int64      `db:"total_clicks"`
}

func (row adminUserRow) toService() *service.AdminUser {
	return &service.AdminUser{
		ID:          row.ID,
		Email:       row.Email,
		Role:        row.Role,
		QuotaDaily:  row.QuotaDaily,
		DisabledAt:  row.DisabledAt,
		CreatedAt:   row.CreatedAt,
		LinkCount:   row.LinkCount,
		LinksToday:  row.LinksToday,
		TotalClicks: row.TotalClicks,
	}
}

func (r *adminMySQL) ListUsers(ctx context.Context, f service.UserFilter) ([]*service.AdminUser, error) {
	var w filter
	if f.BeforeID > 0 {
		w.add("u.id < ?", f.BeforeID)
	}
	if f.Query != "" {
		w.add("u.email LIKE ?", containsPattern(f.Query))
	}
	switch f.Status {
	case "active":
		w.add("u.disabled_at IS NULL")
	case "disabled":
		w.add("u.disabled_at IS NOT NULL")
	}
	q := `
		SELECT u.id, u.email, u.role, u.quota_daily, u.disabled_at, u.created_at,
		       (SELECT COUNT(*) FROM links l WHERE l.user_id = u.id) AS link_count
		FROM users u
		` + w.where() + `
		ORDER BY u.id DESC
		LIMIT ?`
	var rows []adminUserRow
	if err := r.db.SelectContext(ctx, &rows, q, append(w.args, service.AdminPageSize)...); err != nil {
		return nil, fmt.Errorf("repository: list users: %w", err)
	}
	users := make([]*service.AdminUser, len(rows))
	for i, row := range rows {
		users[i] = row.toService()
	}
	return users, nil
}

func (r *adminMySQL) GetUser(ctx context.Context, id, todaySinceID int64) (*service.AdminUser, error) {
	const q = `
		SELECT u.id, u.email, u.role, u.quota_daily, u.disabled_at, u.created_at,
		       (SELECT COUNT(*) FROM links l WHERE l.user_id = u.id) AS link_count,
		       (SELECT COUNT(*) FROM links l WHERE l.user_id = u.id AND l.id >= ?) AS links_today,
		       (SELECT COALESCE(SUM(l.clicks), 0) FROM links l WHERE l.user_id = u.id) AS total_clicks
		FROM users u
		WHERE u.id = ?`
	var row adminUserRow
	if err := r.db.GetContext(ctx, &row, q, todaySinceID, id); err != nil {
		if err = notFound(err); errors.Is(err, service.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("repository: get user: %w", err)
	}
	return row.toService(), nil
}

func (r *adminMySQL) SetUserQuota(ctx context.Context, actorID, userID int64, quota int, reason string) error {
	return r.inTx(ctx, func(tx *sqlx.Tx) error {
		var before int
		if err := tx.GetContext(ctx, &before, `SELECT quota_daily FROM users WHERE id = ? FOR UPDATE`, userID); err != nil {
			return notFound(err)
		}
		if before == quota {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET quota_daily = ? WHERE id = ?`, quota, userID); err != nil {
			return fmt.Errorf("repository: set quota: %w", err)
		}
		return insertAudit(ctx, tx, actorID, "user.quota", "user", userID, reason,
			map[string]any{"before": before, "after": quota})
	})
}

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
		q, action := `UPDATE users SET disabled_at = NULL WHERE id = ?`, "user.enable"
		if disabled {
			q, action = `UPDATE users SET disabled_at = UTC_TIMESTAMP() WHERE id = ?`, "user.disable"
		}
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return fmt.Errorf("repository: set user disabled: %w", err)
		}
		changed = true
		return insertAudit(ctx, tx, actorID, action, "user", userID, reason, nil)
	})
	return changed, err
}

func (r *adminMySQL) LinkCodesByUser(ctx context.Context, userID int64) ([]string, error) {
	var codes []string
	if err := r.db.SelectContext(ctx, &codes, `SELECT short_code FROM links WHERE user_id = ?`, userID); err != nil {
		return nil, fmt.Errorf("repository: link codes by user: %w", err)
	}
	return codes, nil
}

func (r *adminMySQL) SetRole(ctx context.Context, actorID int64, email, role string) (bool, error) {
	var changed bool
	err := r.inTx(ctx, func(tx *sqlx.Tx) error {
		var row struct {
			ID   int64  `db:"id"`
			Role string `db:"role"`
		}
		if err := tx.GetContext(ctx, &row, `SELECT id, role FROM users WHERE email = ? FOR UPDATE`, email); err != nil {
			return notFound(err)
		}
		if row.Role == role {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, row.ID); err != nil {
			return fmt.Errorf("repository: set role: %w", err)
		}
		changed = true
		action := "admin.revoke"
		if role == service.RoleAdmin {
			action = "admin.grant"
		}
		return insertAudit(ctx, tx, actorID, action, "user", row.ID, "", nil)
	})
	return changed, err
}

// --- links ---

const adminLinkColumns = `
	l.id, l.short_code, l.original_url, l.clicks, l.disabled_at, l.expires_at, l.created_at,
	l.user_id, COALESCE(u.email, '') AS owner_email, u.disabled_at AS owner_disabled_at`

type adminLinkRow struct {
	ID              int64      `db:"id"`
	ShortCode       string     `db:"short_code"`
	OriginalURL     string     `db:"original_url"`
	Clicks          int64      `db:"clicks"`
	DisabledAt      *time.Time `db:"disabled_at"`
	ExpiresAt       *time.Time `db:"expires_at"`
	CreatedAt       time.Time  `db:"created_at"`
	OwnerID         int64      `db:"user_id"`
	OwnerEmail      string     `db:"owner_email"`
	OwnerDisabledAt *time.Time `db:"owner_disabled_at"`
}

func (row adminLinkRow) toService() *service.AdminLink {
	return &service.AdminLink{
		ID:              row.ID,
		ShortCode:       row.ShortCode,
		OriginalURL:     row.OriginalURL,
		Clicks:          row.Clicks,
		DisabledAt:      row.DisabledAt,
		ExpiresAt:       row.ExpiresAt,
		CreatedAt:       row.CreatedAt,
		OwnerID:         row.OwnerID,
		OwnerEmail:      row.OwnerEmail,
		OwnerDisabledAt: row.OwnerDisabledAt,
	}
}

func (r *adminMySQL) ListLinks(ctx context.Context, f service.LinkFilter) ([]*service.AdminLink, error) {
	var w filter
	if f.BeforeID > 0 {
		w.add("l.id < ?", f.BeforeID)
	}
	if f.UserID > 0 {
		w.add("l.user_id = ?", f.UserID)
	}
	if f.Query != "" {
		w.add("(l.short_code = ? OR l.original_url LIKE ?)", f.Query, containsPattern(f.Query))
	}
	switch f.Status {
	case "active":
		w.add("l.disabled_at IS NULL AND u.disabled_at IS NULL AND (l.expires_at IS NULL OR l.expires_at > UTC_TIMESTAMP())")
	case "disabled":
		w.add("(l.disabled_at IS NOT NULL OR u.disabled_at IS NOT NULL)")
	case "expired":
		w.add("l.expires_at IS NOT NULL AND l.expires_at <= UTC_TIMESTAMP()")
	}
	q := `SELECT ` + adminLinkColumns + `
		FROM links l LEFT JOIN users u ON u.id = l.user_id
		` + w.where() + `
		ORDER BY l.id DESC
		LIMIT ?`
	var rows []adminLinkRow
	if err := r.db.SelectContext(ctx, &rows, q, append(w.args, service.AdminPageSize)...); err != nil {
		return nil, fmt.Errorf("repository: list links: %w", err)
	}
	links := make([]*service.AdminLink, len(rows))
	for i, row := range rows {
		links[i] = row.toService()
	}
	return links, nil
}

func (r *adminMySQL) GetLink(ctx context.Context, code string) (*service.AdminLink, error) {
	q := `SELECT ` + adminLinkColumns + `
		FROM links l LEFT JOIN users u ON u.id = l.user_id
		WHERE l.short_code = ?`
	var row adminLinkRow
	if err := r.db.GetContext(ctx, &row, q, code); err != nil {
		if err = notFound(err); errors.Is(err, service.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("repository: get link: %w", err)
	}
	return row.toService(), nil
}

func (r *adminMySQL) SetLinkDisabled(ctx context.Context, actorID int64, code string, disabled bool, reason string) (bool, error) {
	var changed bool
	err := r.inTx(ctx, func(tx *sqlx.Tx) error {
		var row struct {
			ID         int64      `db:"id"`
			DisabledAt *time.Time `db:"disabled_at"`
		}
		if err := tx.GetContext(ctx, &row, `SELECT id, disabled_at FROM links WHERE short_code = ? FOR UPDATE`, code); err != nil {
			return notFound(err)
		}
		if (row.DisabledAt != nil) == disabled {
			return nil
		}
		q, action := `UPDATE links SET disabled_at = NULL WHERE id = ?`, "link.enable"
		if disabled {
			q, action = `UPDATE links SET disabled_at = UTC_TIMESTAMP() WHERE id = ?`, "link.disable"
		}
		if _, err := tx.ExecContext(ctx, q, row.ID); err != nil {
			return fmt.Errorf("repository: set link disabled: %w", err)
		}
		changed = true
		return insertAudit(ctx, tx, actorID, action, "link", row.ID, reason, map[string]any{"code": code})
	})
	return changed, err
}

func (r *adminMySQL) DeleteLink(ctx context.Context, actorID int64, code string, reason string) error {
	return r.inTx(ctx, func(tx *sqlx.Tx) error {
		var id int64
		if err := tx.GetContext(ctx, &id, `SELECT id FROM links WHERE short_code = ? FOR UPDATE`, code); err != nil {
			return notFound(err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, id); err != nil {
			return fmt.Errorf("repository: delete link: %w", err)
		}
		return insertAudit(ctx, tx, actorID, "link.delete", "link", id, reason, map[string]any{"code": code})
	})
}

// --- audit ---

func (r *adminMySQL) ListAudit(ctx context.Context, f service.AuditFilter) ([]*service.AuditEntry, error) {
	var w filter
	if f.BeforeID > 0 {
		w.add("a.id < ?", f.BeforeID)
	}
	if f.ActorID > 0 {
		w.add("a.user_id = ?", f.ActorID)
	}
	if f.ResourceType != "" {
		w.add("a.resource_type = ?", f.ResourceType)
	}
	q := `
		SELECT a.id, a.user_id, COALESCE(u.email, '') AS actor_email, a.action, a.resource_type,
		       COALESCE(a.resource_id, 0) AS resource_id,
		       CAST(COALESCE(a.metadata, JSON_OBJECT()) AS CHAR) AS metadata, a.created_at
		FROM audit_log a LEFT JOIN users u ON u.id = a.user_id
		` + w.where() + `
		ORDER BY a.id DESC
		LIMIT ?`
	var rows []struct {
		ID           int64     `db:"id"`
		ActorID      int64     `db:"user_id"`
		ActorEmail   string    `db:"actor_email"`
		Action       string    `db:"action"`
		ResourceType string    `db:"resource_type"`
		ResourceID   int64     `db:"resource_id"`
		Metadata     string    `db:"metadata"`
		CreatedAt    time.Time `db:"created_at"`
	}
	if err := r.db.SelectContext(ctx, &rows, q, append(w.args, service.AdminPageSize)...); err != nil {
		return nil, fmt.Errorf("repository: list audit: %w", err)
	}
	entries := make([]*service.AuditEntry, len(rows))
	for i, row := range rows {
		entries[i] = &service.AuditEntry{
			ID:           row.ID,
			ActorID:      row.ActorID,
			ActorEmail:   row.ActorEmail,
			Action:       row.Action,
			ResourceType: row.ResourceType,
			ResourceID:   row.ResourceID,
			Metadata:     json.RawMessage(row.Metadata),
			CreatedAt:    row.CreatedAt,
		}
	}
	return entries, nil
}

// --- stats ---

func (r *adminMySQL) Stats(ctx context.Context, since time.Time) (*service.Stats, error) {
	st := &service.Stats{}

	// disabled_links uses the same definition as ListLinks status=disabled: the
	// link or its owner is disabled (both answer 410).
	const totals = `
		SELECT (SELECT COUNT(*) FROM users) AS users,
		       (SELECT COUNT(*) FROM links) AS links,
		       (SELECT COALESCE(SUM(clicks), 0) FROM links) AS clicks,
		       (SELECT COUNT(*) FROM users WHERE disabled_at IS NOT NULL) AS disabled_users,
		       (SELECT COUNT(*) FROM links l LEFT JOIN users u ON u.id = l.user_id
		        WHERE l.disabled_at IS NOT NULL OR u.disabled_at IS NOT NULL) AS disabled_links`
	var t struct {
		Users         int64 `db:"users"`
		Links         int64 `db:"links"`
		Clicks        int64 `db:"clicks"`
		DisabledUsers int64 `db:"disabled_users"`
		DisabledLinks int64 `db:"disabled_links"`
	}
	if err := r.db.GetContext(ctx, &t, totals); err != nil {
		return nil, fmt.Errorf("repository: stats totals: %w", err)
	}
	st.Totals = service.StatsTotals{Users: t.Users, Links: t.Links, Clicks: t.Clicks, DisabledUsers: t.DisabledUsers, DisabledLinks: t.DisabledLinks}

	// The session runs in UTC (see config.OpenMySQL), so DATE() is the UTC day.
	days := map[int64]*service.DayStats{} // keyed by Unix seconds of the UTC midnight
	day := func(d time.Time) *service.DayStats {
		if days[d.Unix()] == nil {
			days[d.Unix()] = &service.DayStats{Date: d.UTC()}
		}
		return days[d.Unix()]
	}
	// users and links have time-ordered snowflake IDs, so "id >= sinceID"
	// turns the date filter into a primary-key range instead of a full scan;
	// created_at still decides the exact day. analytics_daily is read through
	// idx_stat_date (migration 008).
	sinceID := idgen.MinIDAt(since)
	for _, part := range []struct {
		q    string
		args []any
		set  func(d *service.DayStats, n int64)
	}{
		{`SELECT DATE(created_at) AS d, COUNT(*) AS n FROM users WHERE id >= ? AND created_at >= ? GROUP BY d`,
			[]any{sinceID, since}, func(d *service.DayStats, n int64) { d.NewUsers = n }},
		{`SELECT DATE(created_at) AS d, COUNT(*) AS n FROM links WHERE id >= ? AND created_at >= ? GROUP BY d`,
			[]any{sinceID, since}, func(d *service.DayStats, n int64) { d.NewLinks = n }},
		{`SELECT stat_date AS d, SUM(clicks) AS n FROM analytics_daily WHERE stat_date >= ? GROUP BY stat_date`,
			[]any{since}, func(d *service.DayStats, n int64) { d.Clicks = n }},
	} {
		var rows []struct {
			D time.Time `db:"d"`
			N int64     `db:"n"`
		}
		if err := r.db.SelectContext(ctx, &rows, part.q, part.args...); err != nil {
			return nil, fmt.Errorf("repository: stats per day: %w", err)
		}
		for _, row := range rows {
			part.set(day(row.D), row.N)
		}
	}
	for _, d := range days {
		st.Daily = append(st.Daily, *d)
	}
	slices.SortFunc(st.Daily, func(a, b service.DayStats) int { return a.Date.Compare(b.Date) })

	// JOIN drops links deleted since: their clicks stay in the daily totals.
	const top = `
		SELECT l.short_code AS code, SUM(a.clicks) AS clicks
		FROM analytics_daily a
		JOIN links l ON l.id = a.link_id
		WHERE a.stat_date >= ?
		GROUP BY a.link_id, l.short_code
		ORDER BY clicks DESC, a.link_id DESC
		LIMIT ?`
	var tops []struct {
		Code   string `db:"code"`
		Clicks int64  `db:"clicks"`
	}
	if err := r.db.SelectContext(ctx, &tops, top, since, service.TopLinksLimit); err != nil {
		return nil, fmt.Errorf("repository: stats top links: %w", err)
	}
	for _, t := range tops {
		st.TopLinks = append(st.TopLinks, service.TopLink{Code: t.Code, Clicks: t.Clicks})
	}
	return st, nil
}
