package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/april/shorten-link/internal/service"
	"github.com/april/shorten-link/pkg/idgen"
)

// --- mock repository ---

type mockRepo struct {
	createErr    error
	getByCodeFn  func(code string) (*service.Link, error)
	deleteErr    error
	listFn       func() ([]*service.Link, error)
}

func (m *mockRepo) Create(_ context.Context, _ *service.Link) error { return m.createErr }
func (m *mockRepo) GetByCode(_ context.Context, code string) (*service.Link, error) {
	return m.getByCodeFn(code)
}
func (m *mockRepo) Delete(_ context.Context, _ string, _ int64) error { return m.deleteErr }
func (m *mockRepo) ListByUser(_ context.Context, _ int64, _ int64, _ int) ([]*service.Link, error) {
	return m.listFn()
}
func (m *mockRepo) BatchUpdateClicks(_ context.Context, _ map[string]int64) error { return nil }

// --- mock cache ---

type mockCache struct {
	store    map[string]string
	getErr   error
	setErr   error
	delErr   error
}

func newMockCache() *mockCache {
	return &mockCache{store: make(map[string]string)}
}

func (c *mockCache) Get(_ context.Context, code string) (string, error) {
	if c.getErr != nil {
		return "", c.getErr
	}
	v, ok := c.store[code]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}
func (c *mockCache) Set(_ context.Context, code, url string, _ time.Duration) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.store[code] = url
	return nil
}
func (c *mockCache) Delete(_ context.Context, code string) error {
	delete(c.store, code)
	return c.delErr
}

// --- helpers ---

func newGen(t *testing.T) *idgen.Generator {
	t.Helper()
	g, err := idgen.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func newSvc(repo service.LinkRepository, cache service.LinkCache) *service.LinkService {
	return service.NewLinkService(repo, cache, nil, service.NoopPublisher{})
}

func newSvcWithGen(repo service.LinkRepository, cache service.LinkCache, gen *idgen.Generator) *service.LinkService {
	return service.NewLinkService(repo, cache, gen, service.NoopPublisher{})
}

// --- Shorten tests ---

func TestShorten_InvalidURL(t *testing.T) {
	svc := newSvc(&mockRepo{}, newMockCache())
	for _, bad := range []string{"ftp://x.com", "not-a-url", "", "javascript:alert(1)"} {
		if _, err := svc.Shorten(context.Background(), 1, bad, nil); !errors.Is(err, service.ErrInvalidURL) {
			t.Errorf("Shorten(%q): want ErrInvalidURL, got %v", bad, err)
		}
	}
}

func TestShorten_RepoError(t *testing.T) {
	repo := &mockRepo{createErr: errors.New("db down")}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))
	_, err := svc.Shorten(context.Background(), 1, "https://example.com", nil)
	if err == nil {
		t.Error("expected error from repo, got nil")
	}
}

func TestShorten_OK(t *testing.T) {
	cache := newMockCache()
	repo := &mockRepo{getByCodeFn: func(code string) (*service.Link, error) {
		return &service.Link{ShortCode: code, OriginalURL: "https://example.com"}, nil
	}}
	svc := newSvcWithGen(repo, cache, newGen(t))

	link, err := svc.Shorten(context.Background(), 1, "https://example.com", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if link.ShortCode == "" {
		t.Error("expected non-empty short code")
	}
	// cache should be warmed
	if _, ok := cache.store[link.ShortCode]; !ok {
		t.Error("cache not warmed after Shorten")
	}
}

// --- Resolve tests (100% branch coverage) ---

func TestResolve_CacheHit(t *testing.T) {
	cache := newMockCache()
	cache.store["abc"] = "https://cached.com"
	repo := &mockRepo{
		getByCodeFn: func(_ string) (*service.Link, error) {
			t.Error("repo should not be called on cache hit")
			return nil, nil
		},
	}
	svc := newSvc(repo, cache)

	url, err := svc.Resolve(context.Background(), "abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != "https://cached.com" {
		t.Errorf("got %q, want https://cached.com", url)
	}
}

func TestResolve_CacheMiss_DBHit(t *testing.T) {
	cache := newMockCache() // empty cache
	repo := &mockRepo{
		getByCodeFn: func(code string) (*service.Link, error) {
			return &service.Link{ShortCode: code, OriginalURL: "https://db.com"}, nil
		},
	}
	svc := newSvc(repo, cache)

	url, err := svc.Resolve(context.Background(), "xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != "https://db.com" {
		t.Errorf("got %q, want https://db.com", url)
	}
	// cache should be warmed after miss
	if cache.store["xyz"] != "https://db.com" {
		t.Error("cache not warmed after DB hit")
	}
}

func TestResolve_CacheMiss_DBMiss(t *testing.T) {
	cache := newMockCache()
	repo := &mockRepo{
		getByCodeFn: func(_ string) (*service.Link, error) {
			return nil, errors.New("not found")
		},
	}
	svc := newSvc(repo, cache)

	_, err := svc.Resolve(context.Background(), "nope")
	if err == nil {
		t.Error("expected error on DB miss, got nil")
	}
}

// --- Delete tests ---

func TestDelete_OK(t *testing.T) {
	cache := newMockCache()
	cache.store["abc"] = "https://x.com"
	repo := &mockRepo{deleteErr: nil}
	svc := newSvc(repo, cache)

	if err := svc.Delete(context.Background(), "abc", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cache.store["abc"]; ok {
		t.Error("cache entry should be removed after Delete")
	}
}

func TestDelete_RepoError(t *testing.T) {
	repo := &mockRepo{deleteErr: errors.New("not found")}
	svc := newSvc(repo, newMockCache())
	if err := svc.Delete(context.Background(), "abc", 1); err == nil {
		t.Error("expected error, got nil")
	}
}

// --- ListByUser tests ---

func TestListByUser_OK(t *testing.T) {
	links := []*service.Link{{ID: 1}, {ID: 2}}
	repo := &mockRepo{listFn: func() ([]*service.Link, error) { return links, nil }}
	svc := newSvc(repo, newMockCache())

	got, err := svc.ListByUser(context.Background(), 1, 0, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 links, got %d", len(got))
	}
}

func TestListByUser_RepoError(t *testing.T) {
	repo := &mockRepo{listFn: func() ([]*service.Link, error) { return nil, errors.New("db err") }}
	svc := newSvc(repo, newMockCache())
	if _, err := svc.ListByUser(context.Background(), 1, 0, 20); err == nil {
		t.Error("expected error, got nil")
	}
}
