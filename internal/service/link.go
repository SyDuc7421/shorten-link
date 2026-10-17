package service

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by repositories when a resource does not exist.
var ErrNotFound = errors.New("not found")

// ErrForbiddenURL is returned when a URL targets a private/internal address (SSRF guard).
var ErrForbiddenURL = errors.New("service: URL targets a forbidden address")

// ErrInvalidExpiry is returned when expires_at is not in the future.
var ErrInvalidExpiry = errors.New("service: expires_at must be in the future")

// ErrInvalidAlias is returned when a custom alias has the wrong format or is reserved.
var ErrInvalidAlias = errors.New("service: alias must be 3-16 characters of A-Z, a-z, 0-9, _ or - and not reserved")

// ErrCodeTaken is returned by repositories when a short code is already in use.
var ErrCodeTaken = errors.New("service: short code already taken")

// ErrQuotaExceeded is returned when a user has created quota_daily links today (UTC).
var ErrQuotaExceeded = errors.New("service: daily link quota exceeded")

// Link is the core domain model for a shortened URL.
type Link struct {
	ID          int64
	UserID      int64
	ShortCode   string
	OriginalURL string
	Clicks      int64
	ExpiresAt   *time.Time
	DisabledAt  *time.Time // set while an admin has disabled the link
	// OwnerDisabled is true when the owner's account is disabled. Only
	// GetByCode fills it.
	OwnerDisabled bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// LinkRepository is the persistence interface, defined here at the consumer site.
type LinkRepository interface {
	Create(ctx context.Context, link *Link) error
	GetByCode(ctx context.Context, code string) (*Link, error)
	Delete(ctx context.Context, code string, userID int64) error
	ListByUser(ctx context.Context, userID int64, afterID int64, limit int) ([]*Link, error)
	BatchUpdateClicks(ctx context.Context, counts map[string]int64) error
	// QuotaUsage returns how many links userID created with id >= sinceID, and
	// the user's quota_daily. ErrNotFound if the user does not exist.
	QuotaUsage(ctx context.Context, userID, sinceID int64) (used, limit int, err error)
}

// LinkCache is the cache interface, defined here at the consumer site.
type LinkCache interface {
	Get(ctx context.Context, code string) (string, error)
	Set(ctx context.Context, code string, url string, ttl time.Duration) error
	Delete(ctx context.Context, code string) error
}
