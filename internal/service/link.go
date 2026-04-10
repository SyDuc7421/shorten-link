package service

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by repositories when a resource does not exist.
var ErrNotFound = errors.New("not found")

// Link is the core domain model for a shortened URL.
type Link struct {
	ID          int64
	UserID      int64
	ShortCode   string
	OriginalURL string
	Clicks      int64
	ExpiresAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// LinkRepository is the persistence interface, defined here at the consumer site.
type LinkRepository interface {
	Create(ctx context.Context, link *Link) error
	GetByCode(ctx context.Context, code string) (*Link, error)
	Delete(ctx context.Context, code string, userID int64) error
	ListByUser(ctx context.Context, userID int64, afterID int64, limit int) ([]*Link, error)
	BatchUpdateClicks(ctx context.Context, counts map[string]int64) error
}

// LinkCache is the cache interface, defined here at the consumer site.
type LinkCache interface {
	Get(ctx context.Context, code string) (string, error)
	Set(ctx context.Context, code string, url string, ttl time.Duration) error
	Delete(ctx context.Context, code string) error
}
