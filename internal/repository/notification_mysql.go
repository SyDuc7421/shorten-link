package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/april/shorten-link/internal/service"
)

type notificationMySQL struct {
	db *sqlx.DB
}

// NewNotificationMySQL returns a NotificationRepository backed by MySQL.
func NewNotificationMySQL(db *sqlx.DB) service.NotificationRepository {
	return &notificationMySQL{db: db}
}

func (r *notificationMySQL) Insert(ctx context.Context, n *service.Notification, dedupeKey string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO notifications (user_id, type, link_id, short_code, expires_at, dedupe_key)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE dedupe_key = dedupe_key`,
		n.UserID, n.Type, n.LinkID, n.ShortCode, n.ExpiresAt, dedupeKey)
	if err != nil {
		return fmt.Errorf("repository: insert notification: %w", err)
	}
	return nil
}

func (r *notificationMySQL) ListByUser(ctx context.Context, userID, beforeID int64, limit int) ([]*service.Notification, error) {
	var rows []struct {
		ID        int64      `db:"id"`
		UserID    int64      `db:"user_id"`
		Type      string     `db:"type"`
		LinkID    *int64     `db:"link_id"`
		ShortCode *string    `db:"short_code"`
		ExpiresAt *time.Time `db:"expires_at"`
		ReadAt    *time.Time `db:"read_at"`
		CreatedAt time.Time  `db:"created_at"`
	}
	q := `SELECT id, user_id, type, link_id, short_code, expires_at, read_at, created_at
	      FROM notifications WHERE user_id = ?`
	args := []any{userID}
	if beforeID > 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	if err := r.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("repository: list notifications: %w", err)
	}
	out := make([]*service.Notification, len(rows))
	for i, x := range rows {
		out[i] = &service.Notification{
			ID: x.ID, UserID: x.UserID, Type: x.Type, LinkID: x.LinkID, ShortCode: x.ShortCode,
			ExpiresAt: x.ExpiresAt, ReadAt: x.ReadAt, CreatedAt: x.CreatedAt,
		}
	}
	return out, nil
}

func (r *notificationMySQL) CountUnread(ctx context.Context, userID int64) (int64, error) {
	var n int64
	if err := r.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL`, userID); err != nil {
		return 0, fmt.Errorf("repository: count unread notifications: %w", err)
	}
	return n, nil
}

// MarkRead uses COALESCE so a second call keeps the first read_at. MySQL
// reports only changed rows, so a second call affects 0 rows; a separate
// existence check tells that apart from a missing or foreign id.
func (r *notificationMySQL) MarkRead(ctx context.Context, userID, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE notifications SET read_at = COALESCE(read_at, UTC_TIMESTAMP()) WHERE id = ? AND user_id = ?`,
		id, userID)
	if err != nil {
		return fmt.Errorf("repository: mark notification read: %w", err)
	}
	var exists int
	if err := r.db.GetContext(ctx, &exists,
		`SELECT COUNT(*) FROM notifications WHERE id = ? AND user_id = ?`, id, userID); err != nil {
		return fmt.Errorf("repository: mark notification read: %w", err)
	}
	if exists == 0 {
		return service.ErrNotFound
	}
	return nil
}

func (r *notificationMySQL) MarkAllRead(ctx context.Context, userID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE notifications SET read_at = UTC_TIMESTAMP() WHERE user_id = ? AND read_at IS NULL`, userID); err != nil {
		return fmt.Errorf("repository: mark all notifications read: %w", err)
	}
	return nil
}

func (r *notificationMySQL) ExpiringLinks(ctx context.Context, now time.Time, window time.Duration, limit int) ([]service.ExpiringLink, error) {
	secs := int64(window / time.Second)
	var rows []struct {
		ID        int64     `db:"id"`
		UserID    int64     `db:"user_id"`
		ShortCode string    `db:"short_code"`
		ExpiresAt time.Time `db:"expires_at"`
	}
	err := r.db.SelectContext(ctx, &rows,
		`SELECT l.id, l.user_id, l.short_code, l.expires_at
		 FROM links l JOIN users u ON u.id = l.user_id
		 WHERE l.expires_at > ? AND l.expires_at <= ?
		   AND TIMESTAMPDIFF(SECOND, l.created_at, l.expires_at) > ?
		   AND l.disabled_at IS NULL AND u.disabled_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.dedupe_key = CONCAT('expiring:', l.id))
		 ORDER BY l.expires_at LIMIT ?`,
		now.UTC(), now.UTC().Add(window), secs, limit)
	if err != nil {
		return nil, fmt.Errorf("repository: expiring links: %w", err)
	}
	out := make([]service.ExpiringLink, len(rows))
	for i, x := range rows {
		out[i] = service.ExpiringLink{ID: x.ID, UserID: x.UserID, ShortCode: x.ShortCode, ExpiresAt: x.ExpiresAt}
	}
	return out, nil
}
