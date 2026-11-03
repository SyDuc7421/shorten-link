package service

import (
	"context"
	"time"
)

const (
	NotificationLinkDisabled    = "link_disabled"
	NotificationLinkEnabled     = "link_enabled"
	NotificationLinkExpiring    = "link_expiring"
	NotificationPasswordChanged = "password_changed"

	RouteLinkDisabled    = "link.disabled"
	RouteLinkEnabled     = "link.enabled"
	RouteLinkExpiring    = "link.expiring"
	RoutePasswordChanged = "user.password_changed"

	ExpiryWarningWindow  = 24 * time.Hour
	NotificationPageSize = 20
)

// NotificationEvent is the JSON body of every notification event.
type NotificationEvent struct {
	DedupeKey  string     `json:"dedupe_key"`
	UserID     int64      `json:"user_id"`
	LinkID     int64      `json:"link_id,omitempty"`    // 0 for password_changed
	ShortCode  string     `json:"short_code,omitempty"` // "" for password_changed
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // link.expiring only
	OccurredAt time.Time  `json:"occurred_at"`
}

// NotificationTypeForRoute maps a routing key to its type; ok=false if unknown.
func NotificationTypeForRoute(routingKey string) (typ string, ok bool) {
	switch routingKey {
	case RouteLinkDisabled:
		return NotificationLinkDisabled, true
	case RouteLinkEnabled:
		return NotificationLinkEnabled, true
	case RouteLinkExpiring:
		return NotificationLinkExpiring, true
	case RoutePasswordChanged:
		return NotificationPasswordChanged, true
	}
	return "", false
}

type Notification struct {
	ID        int64
	UserID    int64
	Type      string
	LinkID    *int64  // nil for password_changed
	ShortCode *string // nil for password_changed
	ExpiresAt *time.Time
	ReadAt    *time.Time
	CreatedAt time.Time
}

// ExpiringLink is a link the scanner should warn about.
type ExpiringLink struct {
	ID        int64
	UserID    int64
	ShortCode string
	ExpiresAt time.Time
}

type NotificationRepository interface {
	// Insert stores n (ID and CreatedAt are ignored, set by MySQL). A row with
	// the same dedupeKey already present is not an error and changes nothing.
	Insert(ctx context.Context, n *Notification, dedupeKey string) error
	// ListByUser returns up to limit rows of userID, newest first, with id < beforeID (0 = from the newest).
	ListByUser(ctx context.Context, userID, beforeID int64, limit int) ([]*Notification, error)
	CountUnread(ctx context.Context, userID int64) (int64, error)
	// MarkRead sets read_at if it is empty. ErrNotFound if id is missing or not userID's.
	MarkRead(ctx context.Context, userID, id int64) error
	MarkAllRead(ctx context.Context, userID int64) error
	// ExpiringLinks returns links with now < expires_at <= now+window,
	// expires_at - created_at > window, link and owner not disabled, and no
	// notification with dedupe_key "expiring:<id>". At most limit rows.
	ExpiringLinks(ctx context.Context, now time.Time, window time.Duration, limit int) ([]ExpiringLink, error)
}
