package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"github.com/april/shorten-link/internal/service"
)

type linkMySQL struct {
	db *sqlx.DB
}

// NewLinkMySQL returns a LinkRepository backed by MySQL.
func NewLinkMySQL(db *sqlx.DB) service.LinkRepository {
	return &linkMySQL{db: db}
}

func (r *linkMySQL) Create(ctx context.Context, link *service.Link) error {
	const q = `
		INSERT INTO links (id, user_id, short_code, original_url, expires_at)
		VALUES (:id, :user_id, :short_code, :original_url, :expires_at)`
	_, err := r.db.NamedExecContext(ctx, q, map[string]any{
		"id":           link.ID,
		"user_id":      link.UserID,
		"short_code":   link.ShortCode,
		"original_url": link.OriginalURL,
		"expires_at":   link.ExpiresAt,
	})
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == mysqlErrDuplicateEntry {
		return service.ErrCodeTaken
	}
	if err != nil {
		return fmt.Errorf("repository: create link: %w", err)
	}
	return nil
}

func (r *linkMySQL) GetByCode(ctx context.Context, code string) (*service.Link, error) {
	const q = `
		SELECT id, user_id, short_code, original_url, clicks, expires_at, created_at, updated_at
		FROM links
		WHERE short_code = ?`
	var row struct {
		ID          int64      `db:"id"`
		UserID      int64      `db:"user_id"`
		ShortCode   string     `db:"short_code"`
		OriginalURL string     `db:"original_url"`
		Clicks      int64      `db:"clicks"`
		ExpiresAt   *time.Time `db:"expires_at"`
		CreatedAt   time.Time  `db:"created_at"`
		UpdatedAt   time.Time  `db:"updated_at"`
	}
	if err := r.db.GetContext(ctx, &row, q, code); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrNotFound
		}
		return nil, fmt.Errorf("repository: get by code: %w", err)
	}

	link := &service.Link{
		ID:          row.ID,
		UserID:      row.UserID,
		ShortCode:   row.ShortCode,
		OriginalURL: row.OriginalURL,
		Clicks:      row.Clicks,
		ExpiresAt:   row.ExpiresAt,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
	return link, nil
}

func (r *linkMySQL) Delete(ctx context.Context, code string, userID int64) error {
	const q = `DELETE FROM links WHERE short_code = ? AND user_id = ?`
	res, err := r.db.ExecContext(ctx, q, code, userID)
	if err != nil {
		return fmt.Errorf("repository: delete link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("repository: delete link rows affected: %w", err)
	}
	if n == 0 {
		return service.ErrNotFound
	}
	return nil
}

func (r *linkMySQL) ListByUser(ctx context.Context, userID int64, afterID int64, limit int) ([]*service.Link, error) {
	const q = `
		SELECT id, user_id, short_code, original_url, clicks, expires_at, created_at, updated_at
		FROM links
		WHERE user_id = ? AND id > ?
		ORDER BY id ASC
		LIMIT ?`
	rows, err := r.db.QueryxContext(ctx, q, userID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("repository: list by user: %w", err)
	}
	defer rows.Close()

	var links []*service.Link
	for rows.Next() {
		var row struct {
			ID          int64      `db:"id"`
			UserID      int64      `db:"user_id"`
			ShortCode   string     `db:"short_code"`
			OriginalURL string     `db:"original_url"`
			Clicks      int64      `db:"clicks"`
			ExpiresAt   *time.Time `db:"expires_at"`
			CreatedAt   time.Time  `db:"created_at"`
			UpdatedAt   time.Time  `db:"updated_at"`
		}
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("repository: scan link: %w", err)
		}
		links = append(links, &service.Link{
			ID:          row.ID,
			UserID:      row.UserID,
			ShortCode:   row.ShortCode,
			OriginalURL: row.OriginalURL,
			Clicks:      row.Clicks,
			ExpiresAt:   row.ExpiresAt,
			CreatedAt:   row.CreatedAt,
			UpdatedAt:   row.UpdatedAt,
		})
	}
	return links, rows.Err()
}

func (r *linkMySQL) QuotaUsage(ctx context.Context, userID, sinceID int64) (service.Quota, error) {
	// idx_user_id holds (user_id, id), so the count is an index range scan.
	const q = `
		SELECT u.quota_daily,
		       u.disabled_at IS NOT NULL AS disabled,
		       (SELECT COUNT(*) FROM links l WHERE l.user_id = u.id AND l.id >= ?) AS used
		FROM users u
		WHERE u.id = ?`
	var row struct {
		Limit    int  `db:"quota_daily"`
		Disabled bool `db:"disabled"`
		Used     int  `db:"used"`
	}
	if err := r.db.GetContext(ctx, &row, q, sinceID, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.Quota{}, service.ErrNotFound
		}
		return service.Quota{}, fmt.Errorf("repository: quota usage: %w", err)
	}
	return service.Quota{Used: row.Used, Limit: row.Limit, Disabled: row.Disabled}, nil
}

func (r *linkMySQL) BatchUpdateClicks(ctx context.Context, counts map[string]int64) error {
	if len(counts) == 0 {
		return nil
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("repository: batch update clicks begin tx: %w", err)
	}
	const q = `UPDATE links SET clicks = clicks + ? WHERE short_code = ?`
	for code, delta := range counts {
		if _, err := tx.ExecContext(ctx, q, delta, code); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("repository: batch update clicks %q: %w", code, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("repository: batch update clicks commit: %w", err)
	}
	return nil
}
