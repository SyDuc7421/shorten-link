package service_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/april/shorten-link/internal/service"
	"github.com/april/shorten-link/pkg/idgen"
)

// --- mock user repository ---

type mockUserRepo struct {
	users  map[string]*service.User
	createErr error
}

func newMockUserRepo() *mockUserRepo {
	return &mockUserRepo{users: make(map[string]*service.User)}
}

func (m *mockUserRepo) Create(_ context.Context, u *service.User) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.users[u.Email] = u
	return nil
}

func (m *mockUserRepo) GetByEmail(_ context.Context, email string) (*service.User, error) {
	if u, ok := m.users[email]; ok {
		return u, nil
	}
	return nil, service.ErrNotFound
}

func (m *mockUserRepo) GetByID(_ context.Context, id int64) (*service.User, error) {
	for _, u := range m.users {
		if u.ID == id {
			return u, nil
		}
	}
	return nil, service.ErrNotFound
}

// --- mock token cache ---

type mockTokenCache struct {
	tokens map[string]int64
}

func newMockTokenCache() *mockTokenCache {
	return &mockTokenCache{tokens: make(map[string]int64)}
}

func (m *mockTokenCache) StoreRefreshToken(_ context.Context, id string, userID int64, _ time.Duration) error {
	m.tokens[id] = userID
	return nil
}

func (m *mockTokenCache) GetRefreshToken(_ context.Context, id string) (int64, error) {
	uid, ok := m.tokens[id]
	if !ok {
		return 0, service.ErrNotFound
	}
	return uid, nil
}

func (m *mockTokenCache) DeleteRefreshToken(_ context.Context, id string) error {
	delete(m.tokens, id)
	return nil
}

func (m *mockTokenCache) IncrLockout(_ context.Context, email string, _ time.Duration) (int64, error) {
	m.tokens["lockout:"+email]++
	return m.tokens["lockout:"+email], nil
}

func (m *mockTokenCache) DeleteLockout(_ context.Context, email string) error {
	delete(m.tokens, "lockout:"+email)
	return nil
}

// --- helpers ---

func newAuthSvc(t *testing.T, userRepo service.UserRepository, tokenCache service.TokenCache) *service.AuthService {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := idgen.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}
	return service.NewAuthService(userRepo, tokenCache, gen, key, &key.PublicKey)
}

// --- Register tests ---

func TestRegister_OK(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	pair, err := svc.Register(context.Background(), "a@b.com", "secret123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Error("expected non-empty tokens")
	}
}

func TestRegister_DuplicateEmail(t *testing.T) {
	repo := newMockUserRepo()
	svc := newAuthSvc(t, repo, newMockTokenCache())
	svc.Register(context.Background(), "a@b.com", "pass1") //nolint:errcheck
	_, err := svc.Register(context.Background(), "a@b.com", "pass2")
	if !errors.Is(err, service.ErrEmailTaken) {
		t.Errorf("want ErrEmailTaken, got %v", err)
	}
}

// --- Login tests ---

func TestLogin_OK(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	svc.Register(context.Background(), "a@b.com", "secret123") //nolint:errcheck
	pair, err := svc.Login(context.Background(), "a@b.com", "secret123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pair.AccessToken == "" {
		t.Error("expected access token")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	svc.Register(context.Background(), "a@b.com", "secret123") //nolint:errcheck
	_, err := svc.Login(context.Background(), "a@b.com", "wrongpass")
	if !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("want ErrInvalidCredentials, got %v", err)
	}
}

func TestLogin_UnknownEmail(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	_, err := svc.Login(context.Background(), "nobody@x.com", "pass")
	if !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("want ErrInvalidCredentials, got %v", err)
	}
}

// --- Refresh tests ---

func TestRefresh_OK(t *testing.T) {
	cache := newMockTokenCache()
	svc := newAuthSvc(t, newMockUserRepo(), cache)
	pair, _ := svc.Register(context.Background(), "a@b.com", "pass")

	newPair, err := svc.Refresh(context.Background(), pair.RefreshToken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newPair.AccessToken == "" || newPair.RefreshToken == "" {
		t.Error("expected non-empty tokens after refresh")
	}
	// old refresh token must be consumed
	_, err = svc.Refresh(context.Background(), pair.RefreshToken)
	if !errors.Is(err, service.ErrInvalidToken) {
		t.Errorf("want ErrInvalidToken on reuse, got %v", err)
	}
}

func TestRefresh_InvalidToken(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	_, err := svc.Refresh(context.Background(), "not-a-token")
	if !errors.Is(err, service.ErrInvalidToken) {
		t.Errorf("want ErrInvalidToken, got %v", err)
	}
}

// --- ParseAccessToken tests ---

func TestParseAccessToken_OK(t *testing.T) {
	repo := newMockUserRepo()
	svc := newAuthSvc(t, repo, newMockTokenCache())
	pair, _ := svc.Register(context.Background(), "a@b.com", "pass")

	userID, err := svc.ParseAccessToken(pair.AccessToken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if userID == 0 {
		t.Error("expected non-zero userID")
	}
}

func TestParseAccessToken_Invalid(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	_, err := svc.ParseAccessToken("garbage")
	if !errors.Is(err, service.ErrInvalidToken) {
		t.Errorf("want ErrInvalidToken, got %v", err)
	}
}
