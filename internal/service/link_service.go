package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/april/shorten-link/pkg/idgen"
)

// ErrInvalidURL is returned when the URL scheme is not http or https.
var ErrInvalidURL = errors.New("service: URL must use http or https scheme")

// Publisher publishes domain events (implemented by messaging in Phase 4).
type Publisher interface {
	Publish(ctx context.Context, routingKey string, payload any) error
}

// NoopPublisher is a Publisher that discards all events.
// Used until messaging is wired in Phase 4.
type NoopPublisher struct{}

func (NoopPublisher) Publish(_ context.Context, _ string, _ any) error { return nil }

const cacheTTL = 24 * time.Hour

// LinkService implements business logic for link shortening.
type LinkService struct {
	repo  LinkRepository
	cache LinkCache
	gen   *idgen.Generator
	pub   Publisher
}

// NewLinkService constructs a LinkService.
func NewLinkService(repo LinkRepository, cache LinkCache, gen *idgen.Generator, pub Publisher) *LinkService {
	return &LinkService{repo: repo, cache: cache, gen: gen, pub: pub}
}

// Shorten validates rawURL, generates a short code, persists it, and warms the cache.
func (s *LinkService) Shorten(ctx context.Context, userID int64, rawURL string, expiresAt *time.Time) (*Link, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, ErrInvalidURL
	}

	id := s.gen.NextID()
	code := idgen.ToBase62(id)

	link := &Link{
		ID:          id,
		UserID:      userID,
		ShortCode:   code,
		OriginalURL: rawURL,
		ExpiresAt:   expiresAt,
	}
	if err := s.repo.Create(ctx, link); err != nil {
		return nil, fmt.Errorf("service: shorten: %w", err)
	}
	s.cache.Set(ctx, code, rawURL, cacheTTL) //nolint:errcheck

	go s.pub.Publish(context.Background(), "link.created", map[string]any{"code": code, "user_id": userID}) //nolint:errcheck

	return link, nil
}

// Resolve returns the original URL for a short code.
// Redis-first; falls back to MySQL on miss. Click event is published async.
func (s *LinkService) Resolve(ctx context.Context, code string) (string, error) {
	if rawURL, err := s.cache.Get(ctx, code); err == nil {
		go s.pub.Publish(context.Background(), "link.clicked", map[string]any{"code": code}) //nolint:errcheck
		return rawURL, nil
	}

	link, err := s.repo.GetByCode(ctx, code)
	if err != nil {
		return "", fmt.Errorf("service: resolve: %w", err)
	}
	s.cache.Set(ctx, code, link.OriginalURL, cacheTTL) //nolint:errcheck
	go s.pub.Publish(context.Background(), "link.clicked", map[string]any{"code": code}) //nolint:errcheck

	return link.OriginalURL, nil
}

// Delete removes a link from MySQL and invalidates the cache.
func (s *LinkService) Delete(ctx context.Context, code string, userID int64) error {
	if err := s.repo.Delete(ctx, code, userID); err != nil {
		return fmt.Errorf("service: delete: %w", err)
	}
	s.cache.Delete(ctx, code) //nolint:errcheck
	return nil
}

// ListByUser returns a cursor-paginated list of links for a user (limit 20).
func (s *LinkService) ListByUser(ctx context.Context, userID int64, afterID int64, limit int) ([]*Link, error) {
	links, err := s.repo.ListByUser(ctx, userID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("service: list by user: %w", err)
	}
	return links, nil
}
