package service

import (
	"context"
	"errors"
	"fmt"
	"net"
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

const (
	cacheTTL       = 24 * time.Hour
	publishTimeout = 2 * time.Second
	dnsTimeout     = 2 * time.Second
)

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

	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return nil, ErrInvalidExpiry
	}

	if err := checkSSRF(ctx, u); err != nil {
		return nil, err
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

	ttl := cacheTTL
	if expiresAt != nil {
		if until := time.Until(*expiresAt); until < ttl {
			ttl = until
		}
	}
	s.cache.Set(ctx, code, rawURL, ttl) //nolint:errcheck

	s.publishAsync("link.created", map[string]any{"code": code, "user_id": userID})

	return link, nil
}

// Resolve returns the original URL for a short code.
// Redis-first; falls back to MySQL on miss. Click event is published async.
func (s *LinkService) Resolve(ctx context.Context, code string) (string, error) {
	if rawURL, err := s.cache.Get(ctx, code); err == nil {
		s.publishAsync("link.clicked", map[string]any{"code": code})
		return rawURL, nil
	}

	link, err := s.repo.GetByCode(ctx, code)
	if err != nil {
		return "", fmt.Errorf("service: resolve: %w", err)
	}
	if link.ExpiresAt != nil && time.Now().After(*link.ExpiresAt) {
		return "", ErrNotFound
	}

	ttl := cacheTTL
	if link.ExpiresAt != nil {
		if until := time.Until(*link.ExpiresAt); until < ttl {
			ttl = until
		}
	}
	s.cache.Set(ctx, code, link.OriginalURL, ttl) //nolint:errcheck
	s.publishAsync("link.clicked", map[string]any{"code": code})

	return link.OriginalURL, nil
}

// publishAsync fires an event without blocking the request. The timeout bounds
// how long each goroutine can live if the broker stalls.
func (s *LinkService) publishAsync(routingKey string, payload any) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		s.pub.Publish(ctx, routingKey, payload) //nolint:errcheck
	}()
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

// privateRanges lists CIDR blocks that must not be targeted via shortened links.
var privateRanges = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8", // "this network"; 0.0.0.0 reaches localhost on Linux
		"10.0.0.0/8",
		"100.64.0.0/10", // carrier-grade NAT
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16", // link-local
		"::/128",
		"::1/128",
		"fe80::/10", // IPv6 link-local
		"fc00::/7",  // IPv6 ULA
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}()

// checkSSRF returns ErrForbiddenURL if u targets a private/internal address or
// uses a non-HTTP/S scheme (scheme validation is already done by the caller).
func checkSSRF(ctx context.Context, u *url.URL) error {
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return ErrForbiddenURL
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostname — resolve to check. Unresolvable hosts are rejected.
		ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return ErrInvalidURL
		}
		for _, a := range addrs {
			if isPrivate(a.IP) {
				return ErrForbiddenURL
			}
		}
		return nil
	}
	if isPrivate(ip) {
		return ErrForbiddenURL
	}
	return nil
}

func isPrivate(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}
