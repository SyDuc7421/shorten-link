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
	createErr   error
	getByCodeFn func(code string) (*service.Link, error)
	deleteErr   error
	listFn      func() ([]*service.Link, error)
	createFn    func(link *service.Link) error // overrides createErr when set
	quotaFn     func(userID, sinceID int64) (used, limit int, err error)
}

func (m *mockRepo) Create(_ context.Context, link *service.Link) error {
	if m.createFn != nil {
		return m.createFn(link)
	}
	return m.createErr
}
func (m *mockRepo) GetByCode(_ context.Context, code string) (*service.Link, error) {
	return m.getByCodeFn(code)
}
func (m *mockRepo) Delete(_ context.Context, _ string, _ int64) error { return m.deleteErr }
func (m *mockRepo) ListByUser(_ context.Context, _ int64, _ int64, _ int) ([]*service.Link, error) {
	return m.listFn()
}
func (m *mockRepo) BatchUpdateClicks(_ context.Context, _ map[string]int64) error { return nil }
func (m *mockRepo) QuotaUsage(_ context.Context, userID, sinceID int64) (int, int, error) {
	if m.quotaFn == nil {
		return 0, 100, nil
	}
	return m.quotaFn(userID, sinceID)
}

// --- mock cache ---

type mockCache struct {
	store  map[string]string
	getErr error
	setErr error
	delErr error
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
		if _, err := svc.Shorten(context.Background(), 1, bad, "", nil); !errors.Is(err, service.ErrInvalidURL) {
			t.Errorf("Shorten(%q): want ErrInvalidURL, got %v", bad, err)
		}
	}
}

func TestShorten_RepoError(t *testing.T) {
	repo := &mockRepo{createErr: errors.New("db down")}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))
	_, err := svc.Shorten(context.Background(), 1, "https://example.com", "", nil)
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

	link, err := svc.Shorten(context.Background(), 1, "https://example.com", "", nil)
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

func TestShorten_ForbiddenAddresses(t *testing.T) {
	svc := newSvcWithGen(&mockRepo{}, newMockCache(), newGen(t))
	for _, u := range []string{
		"http://0.0.0.0/",
		"http://[::]/",
		"http://[fe80::1]/",
		"http://100.64.0.1/",
		"http://[::ffff:127.0.0.1]/",
	} {
		if _, err := svc.Shorten(context.Background(), 1, u, "", nil); !errors.Is(err, service.ErrForbiddenURL) {
			t.Errorf("Shorten(%q): want ErrForbiddenURL, got %v", u, err)
		}
	}
}

func TestShorten_UnresolvableHost(t *testing.T) {
	svc := newSvcWithGen(&mockRepo{}, newMockCache(), newGen(t))
	_, err := svc.Shorten(context.Background(), 1, "http://does-not-exist.invalid/", "", nil)
	if !errors.Is(err, service.ErrInvalidURL) {
		t.Errorf("want ErrInvalidURL, got %v", err)
	}
}

func TestShorten_ExpiryInPast(t *testing.T) {
	svc := newSvcWithGen(&mockRepo{}, newMockCache(), newGen(t))
	past := time.Now().Add(-time.Minute)
	_, err := svc.Shorten(context.Background(), 1, "https://example.com", "", &past)
	if !errors.Is(err, service.ErrInvalidExpiry) {
		t.Errorf("want ErrInvalidExpiry, got %v", err)
	}
}

// deadlinePublisher reports whether each Publish call received a context with a deadline.
type deadlinePublisher struct{ hasDeadline chan bool }

func (p deadlinePublisher) Publish(ctx context.Context, _ string, _ any) error {
	_, ok := ctx.Deadline()
	p.hasDeadline <- ok
	return nil
}

func TestResolve_PublishesWithTimeout(t *testing.T) {
	cache := newMockCache()
	cache.store["abc"] = "https://cached.com"
	pub := deadlinePublisher{hasDeadline: make(chan bool, 1)}
	svc := service.NewLinkService(&mockRepo{}, cache, nil, pub)

	if _, err := svc.Resolve(context.Background(), "abc"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	select {
	case ok := <-pub.hasDeadline:
		if !ok {
			t.Error("click event published without a deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("click event not published")
	}
}

// --- custom alias tests ---

func TestShorten_CustomAlias(t *testing.T) {
	cache := newMockCache()
	var saved *service.Link
	repo := &mockRepo{createFn: func(l *service.Link) error { saved = l; return nil }}
	svc := newSvcWithGen(repo, cache, newGen(t))

	link, err := svc.Shorten(context.Background(), 1, "https://example.com", "Sale-2026_x", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if link.ShortCode != "Sale-2026_x" || saved.ShortCode != "Sale-2026_x" {
		t.Errorf("short code = %q (saved %q), want the alias unchanged", link.ShortCode, saved.ShortCode)
	}
	if _, ok := cache.store["Sale-2026_x"]; !ok {
		t.Error("cache not warmed under the alias")
	}
}

func TestShorten_InvalidAlias(t *testing.T) {
	repo := &mockRepo{createFn: func(*service.Link) error {
		t.Error("repo.Create called for an invalid alias")
		return nil
	}}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))
	for _, alias := range []string{
		"ab",                // too short
		"abcdefghijklmnopq", // 17 chars
		"has space",
		"a/b",
		"a.b",
		"ünïcode",
		"docs", "API", "Healthz", "metrics", // reserved, any case
	} {
		if _, err := svc.Shorten(context.Background(), 1, "https://example.com", alias, nil); !errors.Is(err, service.ErrInvalidAlias) {
			t.Errorf("Shorten(alias %q): want ErrInvalidAlias, got %v", alias, err)
		}
	}
}

func TestShorten_AliasTaken(t *testing.T) {
	calls := 0
	repo := &mockRepo{createFn: func(*service.Link) error { calls++; return service.ErrCodeTaken }}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))

	_, err := svc.Shorten(context.Background(), 1, "https://example.com", "taken", nil)
	if !errors.Is(err, service.ErrCodeTaken) {
		t.Errorf("want ErrCodeTaken, got %v", err)
	}
	if calls != 1 {
		t.Errorf("repo.Create called %d times, want 1 (an alias must not be retried)", calls)
	}
}

func TestShorten_GeneratedCodeCollisionRetries(t *testing.T) {
	var codes []string
	repo := &mockRepo{createFn: func(l *service.Link) error {
		codes = append(codes, l.ShortCode)
		if len(codes) < 3 {
			return service.ErrCodeTaken
		}
		return nil
	}}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))

	link, err := svc.Shorten(context.Background(), 1, "https://example.com", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(codes) != 3 || codes[0] == codes[1] || codes[1] == codes[2] {
		t.Fatalf("attempted codes %v, want 3 distinct", codes)
	}
	if link.ShortCode != codes[2] {
		t.Errorf("returned code %q, want the one that was stored (%q)", link.ShortCode, codes[2])
	}
}

func TestShorten_GeneratedCodeCollisionGivesUp(t *testing.T) {
	calls := 0
	repo := &mockRepo{createFn: func(*service.Link) error { calls++; return service.ErrCodeTaken }}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))

	if _, err := svc.Shorten(context.Background(), 1, "https://example.com", "", nil); !errors.Is(err, service.ErrCodeTaken) {
		t.Errorf("want ErrCodeTaken, got %v", err)
	}
	if calls != 3 {
		t.Errorf("repo.Create called %d times, want 3", calls)
	}
}

func TestCheckAlias(t *testing.T) {
	repo := &mockRepo{getByCodeFn: func(code string) (*service.Link, error) {
		if code == "taken" {
			return &service.Link{ShortCode: code}, nil
		}
		return nil, service.ErrNotFound
	}}
	svc := newSvc(repo, newMockCache())

	for _, tc := range []struct {
		alias, reason string
		available     bool
	}{
		{"free-one", "", true},
		{"Taken", "", true}, // codes are case-sensitive
		{"taken", "taken", false},
		{"ab", "invalid", false},
		{"Docs", "reserved", false},
	} {
		available, reason, err := svc.CheckAlias(context.Background(), tc.alias)
		if err != nil {
			t.Fatalf("CheckAlias(%q): %v", tc.alias, err)
		}
		if available != tc.available || reason != tc.reason {
			t.Errorf("CheckAlias(%q) = (%v, %q), want (%v, %q)", tc.alias, available, reason, tc.available, tc.reason)
		}
	}
}

func TestCheckAlias_RepoError(t *testing.T) {
	repo := &mockRepo{getByCodeFn: func(string) (*service.Link, error) { return nil, errors.New("db down") }}
	svc := newSvc(repo, newMockCache())
	if _, _, err := svc.CheckAlias(context.Background(), "free-one"); err == nil {
		t.Error("expected error, got nil")
	}
}

func TestShorten_QuotaExceeded(t *testing.T) {
	created := false
	repo := &mockRepo{
		quotaFn:  func(_, _ int64) (int, int, error) { return 100, 100, nil },
		createFn: func(_ *service.Link) error { created = true; return nil },
	}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))
	_, err := svc.Shorten(context.Background(), 7, "https://example.com", "", nil)
	if !errors.Is(err, service.ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}
	if created {
		t.Error("link was created despite exceeded quota")
	}
}

func TestShorten_QuotaUnderLimitCreates(t *testing.T) {
	repo := &mockRepo{quotaFn: func(_, _ int64) (int, int, error) { return 99, 100, nil }}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))
	if _, err := svc.Shorten(context.Background(), 7, "https://example.com", "", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestShorten_QuotaCountsFromUTCMidnight(t *testing.T) {
	var gotUser, gotSince int64
	repo := &mockRepo{quotaFn: func(userID, sinceID int64) (int, int, error) {
		gotUser, gotSince = userID, sinceID
		return 0, 100, nil
	}}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))
	if _, err := svc.Shorten(context.Background(), 7, "https://example.com", "", nil); err != nil {
		t.Fatal(err)
	}
	midnight := time.Now().UTC().Truncate(24 * time.Hour)
	if gotUser != 7 || gotSince != idgen.MinIDAt(midnight) {
		t.Errorf("want user 7 since %d, got user %d since %d", idgen.MinIDAt(midnight), gotUser, gotSince)
	}
}

func TestShorten_QuotaRepoError(t *testing.T) {
	repo := &mockRepo{quotaFn: func(_, _ int64) (int, int, error) { return 0, 0, errors.New("db down") }}
	svc := newSvcWithGen(repo, newMockCache(), newGen(t))
	_, err := svc.Shorten(context.Background(), 7, "https://example.com", "", nil)
	if err == nil || errors.Is(err, service.ErrQuotaExceeded) {
		t.Errorf("want plain error, got %v", err)
	}
}
