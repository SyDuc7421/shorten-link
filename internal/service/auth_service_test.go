package service_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/april/shorten-link/internal/service"
	"github.com/april/shorten-link/pkg/idgen"
)

// --- mock user repository ---

type mockUserRepo struct {
	users     map[string]*service.User
	createErr error
	// getByEmailGate, when set, blocks GetByEmail until it is closed.
	getByEmailGate chan struct{}
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
	if m.getByEmailGate != nil {
		<-m.getByEmailGate
	}
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

func (m *mockUserRepo) UpdatePasswordHash(_ context.Context, id int64, hash string) error {
	for _, u := range m.users {
		if u.ID == id {
			u.PasswordHash = hash
			return nil
		}
	}
	return service.ErrNotFound
}

// --- mock token cache ---

type mockTokenCache struct {
	mu sync.Mutex // the reset flow calls it from a background goroutine

	tokens         map[string]int64
	resets         map[string]int64 // token hash -> user ID
	cooldowns      map[string]bool
	revokedFor     []int64
	revokedAt      map[int64]time.Time
	storeResetErrs []error // returned by successive StorePasswordReset calls
	released       []string
	revokeErrs     []error // returned by successive RevokeAllRefreshTokens calls
	onConsume      func()  // runs inside ConsumeRefreshToken, to simulate a concurrent request

	resetPointerDeletedFor []int64
}

func newMockTokenCache() *mockTokenCache {
	return &mockTokenCache{
		tokens:    make(map[string]int64),
		resets:    make(map[string]int64),
		cooldowns: make(map[string]bool),
		revokedAt: make(map[int64]time.Time),
	}
}

// RevokeAllRefreshTokens mirrors the Redis version: it records a revoke time
// that blocks tokens authorised before it, and deletes the user's tokens.
func (m *mockTokenCache) RevokeAllRefreshTokens(_ context.Context, userID int64, at time.Time, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.revokeErrs) > 0 {
		err := m.revokeErrs[0]
		m.revokeErrs = m.revokeErrs[1:]
		if err != nil {
			return err
		}
	}
	m.revokedFor = append(m.revokedFor, userID)
	m.revokedAt[userID] = at
	for id, uid := range m.tokens {
		if uid == userID && !strings.HasPrefix(id, "lockout:") {
			delete(m.tokens, id)
		}
	}
	return nil
}

func (m *mockTokenCache) StorePasswordReset(_ context.Context, hash string, userID int64, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.storeResetErrs) > 0 {
		err := m.storeResetErrs[0]
		m.storeResetErrs = m.storeResetErrs[1:]
		if err != nil {
			return err
		}
	}
	for h, id := range m.resets {
		if id == userID {
			delete(m.resets, h)
		}
	}
	m.resets[hash] = userID
	return nil
}

func (m *mockTokenCache) ConsumePasswordReset(_ context.Context, hash string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.resets[hash]
	if !ok {
		return 0, service.ErrNotFound
	}
	delete(m.resets, hash)
	return id, nil
}

// DeletePasswordReset drops only the user -> token pointer, like the Redis version.
func (m *mockTokenCache) DeletePasswordReset(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetPointerDeletedFor = append(m.resetPointerDeletedFor, userID)
	return nil
}

func (m *mockTokenCache) AcquireResetSlot(_ context.Context, email string, _, _ time.Duration, _ int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cooldowns[email] {
		return false, nil
	}
	m.cooldowns[email] = true
	return true, nil
}

func (m *mockTokenCache) ReleaseResetSlot(_ context.Context, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cooldowns, email)
	m.released = append(m.released, email)
	return nil
}

func (m *mockTokenCache) StoreRefreshToken(_ context.Context, id string, userID int64, _ time.Duration, authorizedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if at, ok := m.revokedAt[userID]; ok && !authorizedAt.After(at) {
		return service.ErrInvalidToken
	}
	m.tokens[id] = userID
	return nil
}

func (m *mockTokenCache) ConsumeRefreshToken(_ context.Context, id string) (int64, error) {
	m.mu.Lock()
	uid, ok := m.tokens[id]
	delete(m.tokens, id)
	hook := m.onConsume
	m.mu.Unlock()
	if !ok {
		return 0, service.ErrNotFound
	}
	if hook != nil {
		hook() // outside the lock: it calls back into the cache
	}
	return uid, nil
}

func (m *mockTokenCache) GetLockout(_ context.Context, email string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens["lockout:"+email], nil
}

func (m *mockTokenCache) IncrLockout(_ context.Context, email string, _ time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens["lockout:"+email]++
	return m.tokens["lockout:"+email], nil
}

func (m *mockTokenCache) DeleteLockout(_ context.Context, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	return service.NewAuthService(userRepo, tokenCache, gen, key, &key.PublicKey, newMockMailer(), mustParseResetURL(t, testResetURL))
}

const testResetURL = "https://app.example.com/reset-password"

func mustParseResetURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := service.ParseResetURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// --- mock mailer ---

type sentMail struct{ to, link string }

type mockMailer struct{ sent chan sentMail }

func newMockMailer() *mockMailer { return &mockMailer{sent: make(chan sentMail, 10)} }

func (m *mockMailer) SendPasswordReset(_ context.Context, to, link string) error {
	m.sent <- sentMail{to, link}
	return nil
}

// next waits for the next mail sent in the background.
func (m *mockMailer) next(t *testing.T) sentMail {
	t.Helper()
	select {
	case mail := <-m.sent:
		return mail
	case <-time.After(2 * time.Second):
		t.Fatal("no mail sent")
		return sentMail{}
	}
}

// none checks that no mail is sent within a short grace period.
func (m *mockMailer) none(t *testing.T) {
	t.Helper()
	select {
	case mail := <-m.sent:
		t.Fatalf("unexpected mail to %s", mail.to)
	case <-time.After(100 * time.Millisecond):
	}
}

func newAuthSvcWithMailer(t *testing.T, users *mockUserRepo, tokens *mockTokenCache) (*service.AuthService, *mockMailer) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := idgen.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}
	mailer := newMockMailer()
	return service.NewAuthService(users, tokens, gen, key, &key.PublicKey, mailer, mustParseResetURL(t, testResetURL)), mailer
}

// tokenFromLink extracts ?token= from a mailed reset link.
func tokenFromLink(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse link %q: %v", link, err)
	}
	tok := u.Query().Get("token")
	if tok == "" {
		t.Fatalf("link %q has no token", link)
	}
	return tok
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
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
	svc.Register(context.Background(), "a@b.com", "password1") //nolint:errcheck
	_, err := svc.Register(context.Background(), "a@b.com", "password2")
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

func TestLogin_LockedAccount_RejectsCorrectPassword(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	ctx := context.Background()
	svc.Register(ctx, "a@b.com", "secret123") //nolint:errcheck
	for i := 0; i < 5; i++ {
		svc.Login(ctx, "a@b.com", "wrongpass") //nolint:errcheck
	}
	_, err := svc.Login(ctx, "a@b.com", "secret123")
	if !errors.Is(err, service.ErrAccountLocked) {
		t.Errorf("want ErrAccountLocked for correct password while locked, got %v", err)
	}
}

func TestLogin_UnknownEmail_LocksLikeKnownEmail(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	ctx := context.Background()
	var err error
	for i := 0; i < 5; i++ {
		_, err = svc.Login(ctx, "nobody@x.com", "wrongpass")
	}
	if !errors.Is(err, service.ErrAccountLocked) {
		t.Errorf("want ErrAccountLocked after 5 failures on unknown email, got %v", err)
	}
}

func TestLogin_EmailIsCaseInsensitive(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	ctx := context.Background()
	svc.Register(ctx, "User@B.com", "secret123") //nolint:errcheck
	if _, err := svc.Login(ctx, " user@b.COM ", "secret123"); err != nil {
		t.Errorf("want login ok with different email case, got %v", err)
	}
}

func TestLogin_LockoutSharedAcrossEmailCase(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	ctx := context.Background()
	svc.Register(ctx, "a@b.com", "secret123") //nolint:errcheck
	for _, e := range []string{"a@b.com", "A@b.com", "a@B.com", "A@B.com", "a@b.COM"} {
		svc.Login(ctx, e, "wrongpass") //nolint:errcheck
	}
	_, err := svc.Login(ctx, "a@b.com", "secret123")
	if !errors.Is(err, service.ErrAccountLocked) {
		t.Errorf("want ErrAccountLocked, got %v", err)
	}
}

func TestRegister_ShortPassword(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	_, err := svc.Register(context.Background(), "a@b.com", "short")
	if !errors.Is(err, service.ErrWeakPassword) {
		t.Errorf("want ErrWeakPassword, got %v", err)
	}
}

// --- Refresh tests ---

func TestRefresh_OK(t *testing.T) {
	cache := newMockTokenCache()
	svc := newAuthSvc(t, newMockUserRepo(), cache)
	pair, _ := svc.Register(context.Background(), "a@b.com", "password")

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
	pair, _ := svc.Register(context.Background(), "a@b.com", "password")

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

func TestRegister_PasswordTooLongForBcrypt(t *testing.T) {
	svc := newAuthSvc(t, newMockUserRepo(), newMockTokenCache())
	long := make([]byte, 73)
	for i := range long {
		long[i] = 'a'
	}
	_, err := svc.Register(context.Background(), "a@b.com", string(long))
	if !errors.Is(err, service.ErrWeakPassword) {
		t.Errorf("want ErrWeakPassword, got %v", err)
	}
}

// --- password reset ---

func TestRequestPasswordReset_UnknownEmail(t *testing.T) {
	tokens := newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, newMockUserRepo(), tokens)

	svc.RequestPasswordReset(context.Background(), "nobody@x.com")
	mailer.none(t)
	if len(tokens.resets) != 0 {
		t.Errorf("stored %d reset tokens for an unknown email", len(tokens.resets))
	}
}

func TestRequestPasswordReset_SendsLinkWithHashedToken(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	if _, err := svc.Register(context.Background(), "a@x.com", "secret123"); err != nil {
		t.Fatal(err)
	}

	// Same normalisation as login: case and surrounding spaces do not matter.
	svc.RequestPasswordReset(context.Background(), "  A@X.com ")
	mail := mailer.next(t)
	if mail.to != "a@x.com" {
		t.Errorf("mail to %q, want a@x.com", mail.to)
	}
	if !strings.HasPrefix(mail.link, testResetURL+"?token=") {
		t.Errorf("link %q does not start with %s?token=", mail.link, testResetURL)
	}
	token := tokenFromLink(t, mail.link)
	userID, ok := tokens.resets[sha256Hex(token)]
	if !ok || userID != users.users["a@x.com"].ID {
		t.Errorf("stored resets %v, want the SHA-256 of the mailed token mapped to the user", tokens.resets)
	}
	if _, raw := tokens.resets[token]; raw {
		t.Error("the raw token must not be stored")
	}
}

func TestRequestPasswordReset_Cooldown(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	svc.Register(context.Background(), "a@x.com", "secret123") //nolint:errcheck

	svc.RequestPasswordReset(context.Background(), "a@x.com")
	first := tokenFromLink(t, mailer.next(t).link)
	svc.RequestPasswordReset(context.Background(), "a@x.com")
	mailer.none(t)
	if _, ok := tokens.resets[sha256Hex(first)]; !ok || len(tokens.resets) != 1 {
		t.Errorf("a request during the cooldown must leave the first token alone, resets = %v", tokens.resets)
	}
}

// requestResetToken registers email and returns a fresh reset token for it.
func requestResetToken(t *testing.T, svc *service.AuthService, mailer *mockMailer, email string) string {
	t.Helper()
	if _, err := svc.Register(context.Background(), email, "old-password"); err != nil {
		t.Fatal(err)
	}
	svc.RequestPasswordReset(context.Background(), email)
	return tokenFromLink(t, mailer.next(t).link)
}

func TestResetPassword_WeakPasswordKeepsToken(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	token := requestResetToken(t, svc, mailer, "a@x.com")

	if err := svc.ResetPassword(context.Background(), token, "short"); !errors.Is(err, service.ErrWeakPassword) {
		t.Fatalf("want ErrWeakPassword, got %v", err)
	}
	// The user fixes the password and retries with the same link.
	if err := svc.ResetPassword(context.Background(), token, "new-password"); err != nil {
		t.Errorf("retry with the same link after a weak password: %v", err)
	}
}

func TestResetPassword_InvalidToken(t *testing.T) {
	svc, _ := newAuthSvcWithMailer(t, newMockUserRepo(), newMockTokenCache())
	if err := svc.ResetPassword(context.Background(), "not-a-real-token", "new-password"); !errors.Is(err, service.ErrInvalidToken) {
		t.Errorf("want ErrInvalidToken, got %v", err)
	}
}

func TestResetPassword_OK(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	token := requestResetToken(t, svc, mailer, "a@x.com")
	userID := users.users["a@x.com"].ID
	tokens.tokens["lockout:a@x.com"] = 3

	if err := svc.ResetPassword(context.Background(), token, "new-password"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if n := tokens.tokens["lockout:a@x.com"]; n != 0 {
		t.Errorf("lockout counter = %d after reset, want cleared", n)
	}

	if _, err := svc.Login(context.Background(), "a@x.com", "new-password"); err != nil {
		t.Errorf("login with the new password: %v", err)
	}
	if _, err := svc.Login(context.Background(), "a@x.com", "old-password"); !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("login with the old password: want ErrInvalidCredentials, got %v", err)
	}
	if len(tokens.revokedFor) == 0 || tokens.revokedFor[0] != userID {
		t.Errorf("revoked sessions for %v, want %d", tokens.revokedFor, userID)
	}
	if len(tokens.resetPointerDeletedFor) != 1 || tokens.resetPointerDeletedFor[0] != userID {
		t.Errorf("reset pointer deleted for %v, want [%d]", tokens.resetPointerDeletedFor, userID)
	}
	if err := svc.ResetPassword(context.Background(), token, "another-password"); !errors.Is(err, service.ErrInvalidToken) {
		t.Errorf("second use of the link: want ErrInvalidToken, got %v", err)
	}
}

// A refresh that consumed its old token just before a password reset revoked
// everything must not come out of it with a working token.
func TestRefresh_RacingPasswordReset(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	resetToken := requestResetToken(t, svc, mailer, "a@x.com")
	pair, err := svc.Login(context.Background(), "a@x.com", "old-password")
	if err != nil {
		t.Fatal(err)
	}

	tokens.onConsume = func() {
		tokens.onConsume = nil
		time.Sleep(2 * time.Millisecond)
		if err := svc.ResetPassword(context.Background(), resetToken, "new-password"); err != nil {
			t.Errorf("reset: %v", err)
		}
	}
	if _, err := svc.Refresh(context.Background(), pair.RefreshToken); !errors.Is(err, service.ErrInvalidToken) {
		t.Errorf("refresh racing a reset: want ErrInvalidToken, got %v", err)
	}
}

func TestResetPassword_RevokesBeforeAndAfterChangingPassword(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	token := requestResetToken(t, svc, mailer, "a@x.com")

	if err := svc.ResetPassword(context.Background(), token, "new-password"); err != nil {
		t.Fatal(err)
	}
	if len(tokens.revokedFor) != 2 {
		t.Errorf("revoked %d times, want 2 (before and after the password change)", len(tokens.revokedFor))
	}
}

func TestResetPassword_RevokeFailsBeforeChange_KeepsOldPassword(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	token := requestResetToken(t, svc, mailer, "a@x.com")
	tokens.revokeErrs = []error{errors.New("redis timeout")}

	if err := svc.ResetPassword(context.Background(), token, "new-password"); err == nil {
		t.Fatal("want error when sessions cannot be revoked")
	}
	if _, err := svc.Login(context.Background(), "a@x.com", "old-password"); err != nil {
		t.Errorf("password must be unchanged when the first revoke fails: %v", err)
	}
}

func TestResetPassword_RetriesRevokeAfterChange(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	token := requestResetToken(t, svc, mailer, "a@x.com")
	// first revoke ok, the one after the password change fails once
	tokens.revokeErrs = []error{nil, errors.New("redis timeout")}

	if err := svc.ResetPassword(context.Background(), token, "new-password"); err != nil {
		t.Fatalf("a transient revoke failure after the change should be retried: %v", err)
	}
	if len(tokens.revokedFor) != 2 {
		t.Errorf("successful revokes = %d, want 2", len(tokens.revokedFor))
	}
}

func TestLogin_RacingPasswordReset(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	requestResetToken(t, svc, mailer, "a@x.com")
	// A reset revoked everything just after this login started checking the
	// old password.
	tokens.revokedAt[users.users["a@x.com"].ID] = time.Now().Add(time.Second)

	if _, err := svc.Login(context.Background(), "a@x.com", "old-password"); !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("want ErrInvalidCredentials, got %v", err)
	}
}

// The request must not wait for the user lookup or storage: otherwise
// registered emails take measurably longer than unknown ones, and a storage
// error would surface as a different status code.
func TestRequestPasswordReset_ReturnsBeforeLookup(t *testing.T) {
	users := newMockUserRepo()
	users.getByEmailGate = make(chan struct{})
	defer close(users.getByEmailGate)
	svc, _ := newAuthSvcWithMailer(t, users, newMockTokenCache())

	done := make(chan struct{})
	go func() {
		svc.RequestPasswordReset(context.Background(), "a@x.com")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RequestPasswordReset waited for the user lookup")
	}
}

func TestRequestPasswordReset_StoreFailureReleasesSlot(t *testing.T) {
	users, tokens := newMockUserRepo(), newMockTokenCache()
	svc, mailer := newAuthSvcWithMailer(t, users, tokens)
	svc.Register(context.Background(), "a@x.com", "secret123") //nolint:errcheck
	tokens.storeResetErrs = []error{errors.New("redis timeout")}

	svc.RequestPasswordReset(context.Background(), "a@x.com")
	mailer.none(t)
	// The retry right after the failure must not be swallowed by the cooldown.
	svc.RequestPasswordReset(context.Background(), "a@x.com")
	if mail := mailer.next(t); mail.to != "a@x.com" {
		t.Errorf("retry mailed %q", mail.to)
	}
	if len(tokens.released) != 1 {
		t.Errorf("slot released %d times, want 1", len(tokens.released))
	}
}

func TestParseResetURL(t *testing.T) {
	for _, ok := range []string{
		"https://app.example.com/reset-password",
		"http://localhost:3000/reset-password",
	} {
		if _, err := service.ParseResetURL(ok); err != nil {
			t.Errorf("ParseResetURL(%q): unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"example.com/reset",       // no scheme: mailed as an unclickable link
		"/reset-password",         // relative
		"ftp://example.com/reset", // not http(s)
		"https:///reset-password", // no host
		"https://app.example.com/reset?token=fixed", // would be overwritten silently
		"://bad",
	} {
		if _, err := service.ParseResetURL(bad); err == nil {
			t.Errorf("ParseResetURL(%q): want error", bad)
		}
	}
}

func disableUser(t *testing.T, users *mockUserRepo, email string) {
	t.Helper()
	now := time.Now()
	users.users[email].DisabledAt = &now
}

func TestLogin_DisabledUserCorrectPassword(t *testing.T) {
	users := newMockUserRepo()
	svc := newAuthSvc(t, users, newMockTokenCache())
	svc.Register(context.Background(), "a@b.com", "secret123") //nolint:errcheck
	disableUser(t, users, "a@b.com")

	_, err := svc.Login(context.Background(), "a@b.com", "secret123")
	if !errors.Is(err, service.ErrAccountDisabled) {
		t.Errorf("want ErrAccountDisabled, got %v", err)
	}
}

func TestLogin_DisabledUserWrongPassword(t *testing.T) {
	users := newMockUserRepo()
	svc := newAuthSvc(t, users, newMockTokenCache())
	svc.Register(context.Background(), "a@b.com", "secret123") //nolint:errcheck
	disableUser(t, users, "a@b.com")

	_, err := svc.Login(context.Background(), "a@b.com", "wrongpass")
	if !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("want ErrInvalidCredentials (status must not leak), got %v", err)
	}
}

func TestRefresh_DisabledUser(t *testing.T) {
	users := newMockUserRepo()
	svc := newAuthSvc(t, users, newMockTokenCache())
	pair, _ := svc.Register(context.Background(), "a@b.com", "password")
	disableUser(t, users, "a@b.com")

	_, err := svc.Refresh(context.Background(), pair.RefreshToken)
	if !errors.Is(err, service.ErrInvalidToken) {
		t.Errorf("want ErrInvalidToken, got %v", err)
	}
}
