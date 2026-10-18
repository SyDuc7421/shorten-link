package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/april/shorten-link/internal/service"
)

// fakeAdminRepo keeps just enough state for AdminService tests.
type fakeAdminRepo struct {
	disabledUsers map[int64]bool
	codes         map[int64][]string
	quotas        map[int64]int
	calls         []string
	disableErr    error
	roleEmail     string
	role          string
	stats         *service.Stats
	statsSince    time.Time
	createdAdmin  *service.User
}

func (f *fakeAdminRepo) CreateAdmin(_ context.Context, u *service.User) error {
	f.createdAdmin = u
	return nil
}

func newFakeAdminRepo() *fakeAdminRepo {
	return &fakeAdminRepo{
		disabledUsers: map[int64]bool{},
		codes:         map[int64][]string{},
		quotas:        map[int64]int{},
	}
}

func (f *fakeAdminRepo) ListUsers(context.Context, service.UserFilter) ([]*service.AdminUser, error) {
	return nil, nil
}
func (f *fakeAdminRepo) GetUser(_ context.Context, id, _ int64) (*service.AdminUser, error) {
	u := &service.AdminUser{ID: id, QuotaDaily: f.quotas[id]}
	if f.disabledUsers[id] {
		now := time.Now()
		u.DisabledAt = &now
	}
	return u, nil
}
func (f *fakeAdminRepo) SetUserQuota(_ context.Context, _, userID int64, quota int, _ string) error {
	f.calls = append(f.calls, "quota")
	f.quotas[userID] = quota
	return nil
}
func (f *fakeAdminRepo) SetUserDisabled(_ context.Context, _, userID int64, disabled bool, _ string) (bool, error) {
	f.calls = append(f.calls, "set-disabled")
	if f.disableErr != nil {
		return false, f.disableErr
	}
	changed := f.disabledUsers[userID] != disabled
	f.disabledUsers[userID] = disabled
	return changed, nil
}
func (f *fakeAdminRepo) LinkCodesByUser(_ context.Context, userID int64) ([]string, error) {
	return f.codes[userID], nil
}
func (f *fakeAdminRepo) ListLinks(context.Context, service.LinkFilter) ([]*service.AdminLink, error) {
	return nil, nil
}
func (f *fakeAdminRepo) GetLink(_ context.Context, code string) (*service.AdminLink, error) {
	return &service.AdminLink{ShortCode: code}, nil
}
func (f *fakeAdminRepo) SetLinkDisabled(context.Context, int64, string, bool, string) (bool, error) {
	f.calls = append(f.calls, "link-disabled")
	return true, nil
}
func (f *fakeAdminRepo) DeleteLink(context.Context, int64, string, string) error {
	f.calls = append(f.calls, "link-delete")
	return nil
}
func (f *fakeAdminRepo) ListAudit(context.Context, service.AuditFilter) ([]*service.AuditEntry, error) {
	return nil, nil
}
func (f *fakeAdminRepo) SetRole(_ context.Context, _ int64, email, role string) (bool, error) {
	f.roleEmail, f.role = email, role
	return true, nil
}

func newAdminSvc(repo *fakeAdminRepo, cache *mockCache, tokens *mockTokenCache) *service.AdminService {
	return service.NewAdminService(repo, newMockUserRepo(), cache, tokens)
}

func TestDisableUser_Self(t *testing.T) {
	repo := newFakeAdminRepo()
	svc := newAdminSvc(repo, newMockCache(), newMockTokenCache())
	if _, err := svc.DisableUser(context.Background(), 1, 1, ""); !errors.Is(err, service.ErrSelfDisable) {
		t.Fatalf("want ErrSelfDisable, got %v", err)
	}
	if len(repo.calls) != 0 {
		t.Errorf("repo called: %v", repo.calls)
	}
}

func TestDisableUser_RevokesAndPurges(t *testing.T) {
	repo := newFakeAdminRepo()
	repo.codes[2] = []string{"a", "b"}
	cache, tokens := newMockCache(), newMockTokenCache()
	cache.store["a"], cache.store["b"], cache.store["other"] = "https://e.com", "https://e.com", "https://e.com"
	svc := newAdminSvc(repo, cache, tokens)

	u, err := svc.DisableUser(context.Background(), 1, 2, "spam")
	if err != nil {
		t.Fatal(err)
	}
	if u.DisabledAt == nil {
		t.Error("returned user not disabled")
	}
	if len(tokens.revokedFor) != 1 || tokens.revokedFor[0] != 2 {
		t.Errorf("want sessions of user 2 revoked, got %v", tokens.revokedFor)
	}
	if _, ok := cache.store["a"]; ok || cache.store["other"] == "" {
		t.Errorf("want only the user's codes purged, cache %v", cache.store)
	}
}

func TestDisableUser_RetryWhenAlreadyDisabled(t *testing.T) {
	repo := newFakeAdminRepo()
	repo.disabledUsers[2] = true // first attempt updated the DB, then failed
	repo.codes[2] = []string{"a"}
	cache, tokens := newMockCache(), newMockTokenCache()
	cache.store["a"] = "https://e.com"
	svc := newAdminSvc(repo, cache, tokens)

	if _, err := svc.DisableUser(context.Background(), 1, 2, ""); err != nil {
		t.Fatal(err)
	}
	if len(tokens.revokedFor) != 1 {
		t.Error("retry did not revoke sessions")
	}
	if _, ok := cache.store["a"]; ok {
		t.Error("retry did not purge the cache")
	}
}

func TestDisableUser_RevokeFails(t *testing.T) {
	tokens := newMockTokenCache()
	tokens.revokeErrs = []error{errors.New("redis down")}
	svc := newAdminSvc(newFakeAdminRepo(), newMockCache(), tokens)
	if _, err := svc.DisableUser(context.Background(), 1, 2, ""); err == nil {
		t.Fatal("want error when revoke fails")
	}
}

func TestEnableUser_PurgesMarkers(t *testing.T) {
	repo := newFakeAdminRepo()
	repo.disabledUsers[2] = true
	repo.codes[2] = []string{"a"}
	cache := newMockCache()
	cache.store["a"] = "!disabled"
	svc := newAdminSvc(repo, cache, newMockTokenCache())

	u, err := svc.EnableUser(context.Background(), 1, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.DisabledAt != nil {
		t.Error("returned user still disabled")
	}
	if _, ok := cache.store["a"]; ok {
		t.Error("!disabled marker still cached after enable")
	}
}

func TestSetQuota_Range(t *testing.T) {
	svc := newAdminSvc(newFakeAdminRepo(), newMockCache(), newMockTokenCache())
	for _, q := range []int{-1, 100001} {
		if _, err := svc.SetQuota(context.Background(), 1, 2, q, ""); !errors.Is(err, service.ErrInvalidQuota) {
			t.Errorf("quota %d: want ErrInvalidQuota, got %v", q, err)
		}
	}
	for _, q := range []int{0, 100000} {
		u, err := svc.SetQuota(context.Background(), 1, 2, q, "")
		if err != nil || u.QuotaDaily != q {
			t.Errorf("quota %d: got %+v, %v", q, u, err)
		}
	}
}

func TestReasonTooLong(t *testing.T) {
	repo := newFakeAdminRepo()
	svc := newAdminSvc(repo, newMockCache(), newMockTokenCache())
	long := strings.Repeat("x", 501)
	if _, err := svc.DisableLink(context.Background(), 1, "abc", long); !errors.Is(err, service.ErrReasonTooLong) {
		t.Errorf("DisableLink: want ErrReasonTooLong, got %v", err)
	}
	if _, err := svc.DisableUser(context.Background(), 1, 2, long); !errors.Is(err, service.ErrReasonTooLong) {
		t.Errorf("DisableUser: want ErrReasonTooLong, got %v", err)
	}
	if err := svc.DeleteLink(context.Background(), 1, "abc", long); !errors.Is(err, service.ErrReasonTooLong) {
		t.Errorf("DeleteLink: want ErrReasonTooLong, got %v", err)
	}
	if len(repo.calls) != 0 {
		t.Errorf("repo called: %v", repo.calls)
	}
	if _, err := svc.DisableLink(context.Background(), 1, "abc", strings.Repeat("x", 500)); err != nil {
		t.Errorf("500 bytes must be accepted: %v", err)
	}
}

func TestLinkActions_DeleteCache(t *testing.T) {
	ctx := context.Background()
	for name, act := range map[string]func(*service.AdminService) error{
		"disable": func(s *service.AdminService) error { _, err := s.DisableLink(ctx, 1, "abc", ""); return err },
		"enable":  func(s *service.AdminService) error { _, err := s.EnableLink(ctx, 1, "abc", ""); return err },
		"delete":  func(s *service.AdminService) error { return s.DeleteLink(ctx, 1, "abc", "") },
	} {
		t.Run(name, func(t *testing.T) {
			cache := newMockCache()
			cache.store["abc"] = "https://e.com"
			if err := act(newAdminSvc(newFakeAdminRepo(), cache, newMockTokenCache())); err != nil {
				t.Fatal(err)
			}
			if _, ok := cache.store["abc"]; ok {
				t.Error("cache key not deleted")
			}
		})
	}
}

func TestIsActiveAdmin(t *testing.T) {
	users := newMockUserRepo()
	now := time.Now()
	users.users["admin@x.com"] = &service.User{ID: 1, Role: service.RoleAdmin}
	users.users["user@x.com"] = &service.User{ID: 2, Role: service.RoleUser}
	users.users["off@x.com"] = &service.User{ID: 3, Role: service.RoleAdmin, DisabledAt: &now}
	svc := service.NewAdminService(newFakeAdminRepo(), users, newMockCache(), newMockTokenCache())

	for id, want := range map[int64]bool{1: true, 2: false, 3: false, 99: false} {
		got, err := svc.IsActiveAdmin(context.Background(), id)
		if err != nil || got != want {
			t.Errorf("user %d: want %v, got %v, %v", id, want, got, err)
		}
	}
}

func TestSetAdmin(t *testing.T) {
	repo := newFakeAdminRepo()
	svc := service.NewAdminService(repo, newMockUserRepo(), nil, nil)
	if changed, err := svc.SetAdmin(context.Background(), " A@X.com ", true); err != nil || !changed {
		t.Errorf("want changed, got %v, %v", changed, err)
	}
	if repo.roleEmail != "a@x.com" || repo.role != service.RoleAdmin {
		t.Errorf("want normalized email and admin role, got %q %q", repo.roleEmail, repo.role)
	}
	svc.SetAdmin(context.Background(), "a@x.com", false) //nolint:errcheck
	if repo.role != service.RoleUser {
		t.Errorf("revoke: want role user, got %q", repo.role)
	}
}

func (f *fakeAdminRepo) Stats(_ context.Context, since time.Time) (*service.Stats, error) {
	f.statsSince = since
	if f.stats == nil {
		return &service.Stats{}, nil
	}
	return f.stats, nil
}

func TestStats_DaysRange(t *testing.T) {
	svc := newAdminSvc(newFakeAdminRepo(), newMockCache(), newMockTokenCache())
	for _, d := range []int{0, -1, 91} {
		if _, err := svc.Stats(context.Background(), d); !errors.Is(err, service.ErrInvalidDays) {
			t.Errorf("days %d: want ErrInvalidDays, got %v", d, err)
		}
	}
	for _, d := range []int{1, 90} {
		st, err := svc.Stats(context.Background(), d)
		if err != nil || len(st.Daily) != d {
			t.Errorf("days %d: want %d entries, got %v, %v", d, d, st, err)
		}
	}
}

func TestStats_ZeroFillsAndOrders(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	repo := newFakeAdminRepo()
	repo.stats = &service.Stats{
		Totals: service.StatsTotals{Users: 9},
		Daily: []service.DayStats{
			{Date: today, Clicks: 4},
			{Date: today.AddDate(0, 0, -2), NewUsers: 1},
		},
		TopLinks: []service.TopLink{{Code: "a", Clicks: 4}},
	}
	svc := newAdminSvc(repo, newMockCache(), newMockTokenCache())

	st, err := svc.Stats(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !repo.statsSince.Equal(today.AddDate(0, 0, -2)) {
		t.Errorf("since: want %v, got %v", today.AddDate(0, 0, -2), repo.statsSince)
	}
	want := []service.DayStats{
		{Date: today.AddDate(0, 0, -2), NewUsers: 1},
		{Date: today.AddDate(0, 0, -1)},
		{Date: today, Clicks: 4},
	}
	if len(st.Daily) != 3 {
		t.Fatalf("want 3 days, got %d", len(st.Daily))
	}
	for i := range want {
		if !st.Daily[i].Date.Equal(want[i].Date) || st.Daily[i].NewUsers != want[i].NewUsers || st.Daily[i].Clicks != want[i].Clicks {
			t.Errorf("day %d: want %+v, got %+v", i, want[i], st.Daily[i])
		}
	}
	if st.Totals.Users != 9 || len(st.TopLinks) != 1 {
		t.Errorf("totals/top not passed through: %+v", st)
	}
}

func TestCreateAdmin_NewUser(t *testing.T) {
	repo := newFakeAdminRepo()
	svc := service.NewAdminService(repo, newMockUserRepo(), nil, nil)

	created, err := svc.CreateAdmin(context.Background(), 42, " Admin@X.com ", "s3cret-pass")
	if err != nil || !created {
		t.Fatalf("want created, got %v, %v", created, err)
	}
	u := repo.createdAdmin
	if u == nil || u.ID != 42 || u.Email != "admin@x.com" || u.Role != service.RoleAdmin || u.QuotaDaily != 100 {
		t.Fatalf("unexpected user %+v", u)
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("s3cret-pass")) != nil {
		t.Error("password not hashed with bcrypt")
	}
}

func TestCreateAdmin_ExistingUserIsGrantedPasswordUnchanged(t *testing.T) {
	repo := newFakeAdminRepo()
	users := newMockUserRepo()
	users.users["u@x.com"] = &service.User{ID: 7, Email: "u@x.com", PasswordHash: "old", Role: service.RoleUser}
	svc := service.NewAdminService(repo, users, nil, nil)

	created, err := svc.CreateAdmin(context.Background(), 42, "u@x.com", "") // no password needed
	if err != nil || created {
		t.Fatalf("want granted (not created), got %v, %v", created, err)
	}
	if repo.createdAdmin != nil || repo.roleEmail != "u@x.com" || repo.role != service.RoleAdmin {
		t.Errorf("want SetRole(admin) only, got created=%+v role=%q/%q", repo.createdAdmin, repo.roleEmail, repo.role)
	}
	if users.users["u@x.com"].PasswordHash != "old" {
		t.Error("password of an existing user must not change")
	}
}

func TestCreateAdmin_WeakPassword(t *testing.T) {
	repo := newFakeAdminRepo()
	svc := service.NewAdminService(repo, newMockUserRepo(), nil, nil)
	for _, pw := range []string{"", "short", strings.Repeat("x", 73)} {
		if _, err := svc.CreateAdmin(context.Background(), 42, "new@x.com", pw); !errors.Is(err, service.ErrWeakPassword) {
			t.Errorf("password %q: want ErrWeakPassword, got %v", pw, err)
		}
	}
	if repo.createdAdmin != nil {
		t.Error("user created despite a weak password")
	}
}
