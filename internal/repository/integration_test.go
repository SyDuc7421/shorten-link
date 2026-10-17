package repository_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/april/shorten-link/internal/repository"
	"github.com/april/shorten-link/internal/service"
)

// migrationsDir returns the absolute path to deploy/migrations from any test file location.
func migrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "migrations")
}

func setupMySQL(t *testing.T) *sqlx.DB {
	t.Helper()
	ctx := context.Background()

	c, err := tcmysql.Run(ctx, "mysql:8",
		tcmysql.WithDatabase("shortlink"),
		tcmysql.WithUsername("root"),
		tcmysql.WithPassword("secret"),
	)
	if err != nil {
		t.Fatalf("start mysql container: %v", err)
	}
	t.Cleanup(func() { c.Terminate(ctx) }) //nolint:errcheck

	dsn, err := c.ConnectionString(ctx, "parseTime=true", "multiStatements=true")
	if err != nil {
		t.Fatalf("mysql dsn: %v", err)
	}

	db, err := sqlx.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for i := 0; i < 20; i++ {
		if err := db.PingContext(ctx); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	migs := migrationsDir()
	for _, f := range []string{
		"001_create_users.up.sql",
		"002_create_links.up.sql",
		"003_create_analytics.up.sql",
		"004_links_expires_at_datetime.up.sql",
		"005_links_short_code_case_sensitive.up.sql",
	} {
		b, err := os.ReadFile(filepath.Join(migs, f))
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := db.ExecContext(ctx, string(b)); err != nil {
			t.Fatalf("run migration %s: %v", f, err)
		}
	}

	return db
}

func setupRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()

	c, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { c.Terminate(ctx) }) //nolint:errcheck

	addr, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	// testcontainers-go redis returns "redis://host:port"
	addr = strings.TrimPrefix(addr, "redis://")

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// --- MySQL tests ---

func TestLinkMySQL_CreateAndGet(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash) VALUES (1, 'a@b.com', 'x')`) //nolint:errcheck

	link := &service.Link{
		ID:          1000,
		UserID:      1,
		ShortCode:   "abc1234",
		OriginalURL: "https://example.com",
	}
	if err := repo.Create(ctx, link); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByCode(ctx, "abc1234")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OriginalURL != link.OriginalURL {
		t.Errorf("url mismatch: got %q want %q", got.OriginalURL, link.OriginalURL)
	}
	if got.ShortCode != link.ShortCode {
		t.Errorf("code mismatch: got %q want %q", got.ShortCode, link.ShortCode)
	}
}

func TestLinkMySQL_GetByCode_NotFound(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)

	_, err := repo.GetByCode(context.Background(), "nope")
	if err != service.ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestLinkMySQL_Delete(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash) VALUES (2, 'c@d.com', 'x')`) //nolint:errcheck
	link := &service.Link{ID: 2000, UserID: 2, ShortCode: "del0001", OriginalURL: "https://del.com"}
	repo.Create(ctx, link) //nolint:errcheck

	if err := repo.Delete(ctx, "del0001", 2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.Delete(ctx, "del0001", 2); err != service.ErrNotFound {
		t.Errorf("want ErrNotFound on second delete, got %v", err)
	}
}

func TestLinkMySQL_ListByUser(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash) VALUES (3, 'e@f.com', 'x')`) //nolint:errcheck
	for i := 0; i < 5; i++ {
		l := &service.Link{
			ID:          int64(3000 + i),
			UserID:      3,
			ShortCode:   fmt.Sprintf("list%04d", i),
			OriginalURL: "https://list.com",
		}
		repo.Create(ctx, l) //nolint:errcheck
	}

	links, err := repo.ListByUser(ctx, 3, 0, 20)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(links) != 5 {
		t.Errorf("want 5 links, got %d", len(links))
	}

	// cursor: afterID=3002 → should return IDs 3003, 3004
	links2, err := repo.ListByUser(ctx, 3, 3002, 20)
	if err != nil {
		t.Fatalf("list cursor: %v", err)
	}
	if len(links2) != 2 {
		t.Errorf("want 2 links after cursor, got %d", len(links2))
	}
}

func TestLinkMySQL_BatchUpdateClicks(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash) VALUES (4, 'g@h.com', 'x')`) //nolint:errcheck
	link := &service.Link{ID: 4000, UserID: 4, ShortCode: "click01", OriginalURL: "https://click.com"}
	repo.Create(ctx, link) //nolint:errcheck

	if err := repo.BatchUpdateClicks(ctx, map[string]int64{"click01": 42}); err != nil {
		t.Fatalf("batch update: %v", err)
	}

	got, err := repo.GetByCode(ctx, "click01")
	if err != nil {
		t.Fatalf("get after batch update: %v", err)
	}
	if got.Clicks != 42 {
		t.Errorf("want 42 clicks, got %d", got.Clicks)
	}
}

// --- Redis tests ---

func TestLinkRedis_SetAndGet(t *testing.T) {
	rdb := setupRedis(t)
	cache := repository.NewLinkRedis(rdb)
	ctx := context.Background()

	if err := cache.Set(ctx, "abc", "https://example.com", time.Minute); err != nil {
		t.Fatalf("set: %v", err)
	}

	url, err := cache.Get(ctx, "abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if url != "https://example.com" {
		t.Errorf("url mismatch: got %q", url)
	}
}

func TestLinkRedis_Get_NotFound(t *testing.T) {
	rdb := setupRedis(t)
	cache := repository.NewLinkRedis(rdb)

	_, err := cache.Get(context.Background(), "missing")
	if err != service.ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestLinkRedis_Delete(t *testing.T) {
	rdb := setupRedis(t)
	cache := repository.NewLinkRedis(rdb)
	ctx := context.Background()

	cache.Set(ctx, "todel", "https://x.com", time.Minute) //nolint:errcheck
	if err := cache.Delete(ctx, "todel"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := cache.Get(ctx, "todel"); err != service.ErrNotFound {
		t.Errorf("want ErrNotFound after delete, got %v", err)
	}
}

func TestLinkMySQL_GetByCode_ReturnsTimestamps(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash) VALUES (5, 'i@j.com', 'x')`) //nolint:errcheck
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	link := &service.Link{ID: 5000, UserID: 5, ShortCode: "exp0001", OriginalURL: "https://exp.com", ExpiresAt: &exp}
	if err := repo.Create(ctx, link); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByCode(ctx, "exp0001")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at: got %v want %v", got.ExpiresAt, exp)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
}

func TestLinkMySQL_ListByUser_ReturnsTimestamps(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash) VALUES (6, 'k@l.com', 'x')`) //nolint:errcheck
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	repo.Create(ctx, &service.Link{ID: 6000, UserID: 6, ShortCode: "exp0002", OriginalURL: "https://exp.com", ExpiresAt: &exp}) //nolint:errcheck

	links, err := repo.ListByUser(ctx, 6, 0, 20)
	if err != nil || len(links) != 1 {
		t.Fatalf("list: %v (%d links)", err, len(links))
	}
	if links[0].ExpiresAt == nil || !links[0].ExpiresAt.Equal(exp) {
		t.Errorf("expires_at: got %v want %v", links[0].ExpiresAt, exp)
	}
	if links[0].CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
}

func TestUserMySQL_Create_DuplicateEmail(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewUserMySQL(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &service.User{ID: 1, Email: "dup@x.com", PasswordHash: "h", Plan: "free"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := repo.Create(ctx, &service.User{ID: 2, Email: "dup@x.com", PasswordHash: "h", Plan: "free"})
	if !errors.Is(err, service.ErrEmailTaken) {
		t.Errorf("want ErrEmailTaken, got %v", err)
	}
}

func TestTokenRedis_ConsumeRefreshToken_OnlyOnce(t *testing.T) {
	rdb := setupRedis(t)
	tokens := repository.NewTokenRedis(rdb)
	ctx := context.Background()

	tokens.StoreRefreshToken(ctx, "tid", 42, time.Minute, time.Now()) //nolint:errcheck
	uid, err := tokens.ConsumeRefreshToken(ctx, "tid")
	if err != nil || uid != 42 {
		t.Fatalf("first consume: uid=%d err=%v", uid, err)
	}
	if _, err := tokens.ConsumeRefreshToken(ctx, "tid"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("second consume: want ErrNotFound, got %v", err)
	}
}

func TestTokenRedis_GetLockout(t *testing.T) {
	rdb := setupRedis(t)
	tokens := repository.NewTokenRedis(rdb)
	ctx := context.Background()

	if n, err := tokens.GetLockout(ctx, "a@b.com"); err != nil || n != 0 {
		t.Fatalf("fresh lockout: n=%d err=%v", n, err)
	}
	tokens.IncrLockout(ctx, "a@b.com", time.Minute) //nolint:errcheck
	tokens.IncrLockout(ctx, "a@b.com", time.Minute) //nolint:errcheck
	if n, err := tokens.GetLockout(ctx, "a@b.com"); err != nil || n != 2 {
		t.Errorf("after 2 incr: n=%d err=%v", n, err)
	}
}

func TestLinkMySQL_Create_ExpiryAfter2038(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash) VALUES (7, 'm@n.com', 'x')`) //nolint:errcheck
	exp := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := repo.Create(ctx, &service.Link{ID: 7000, UserID: 7, ShortCode: "y2040", OriginalURL: "https://x.com", ExpiresAt: &exp}); err != nil {
		t.Fatalf("create with 2040 expiry: %v", err)
	}
	got, err := repo.GetByCode(ctx, "y2040")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at: got %v want %v", got.ExpiresAt, exp)
	}
}

func TestLinkMySQL_Create_DuplicateCode(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &service.Link{ID: 8000, UserID: 8, ShortCode: "dup-code", OriginalURL: "https://a.com"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := repo.Create(ctx, &service.Link{ID: 8001, UserID: 9, ShortCode: "dup-code", OriginalURL: "https://b.com"})
	if !errors.Is(err, service.ErrCodeTaken) {
		t.Errorf("want ErrCodeTaken, got %v", err)
	}
}

func TestLinkMySQL_ShortCodeIsCaseSensitive(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &service.Link{ID: 9000, UserID: 9, ShortCode: "Sale", OriginalURL: "https://upper.com"}); err != nil {
		t.Fatalf("create Sale: %v", err)
	}
	if err := repo.Create(ctx, &service.Link{ID: 9001, UserID: 9, ShortCode: "sale", OriginalURL: "https://lower.com"}); err != nil {
		t.Fatalf("create sale (differs only in case): %v", err)
	}
	for code, want := range map[string]string{"Sale": "https://upper.com", "sale": "https://lower.com"} {
		got, err := repo.GetByCode(ctx, code)
		if err != nil {
			t.Fatalf("GetByCode(%q): %v", code, err)
		}
		if got.OriginalURL != want {
			t.Errorf("GetByCode(%q) = %s, want %s", code, got.OriginalURL, want)
		}
	}
	if _, err := repo.GetByCode(ctx, "SALE"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("GetByCode(SALE): want ErrNotFound, got %v", err)
	}
}

// --- password reset and session index ---

func TestTokenRedis_PasswordReset_SingleUse(t *testing.T) {
	cache := repository.NewTokenRedis(setupRedis(t))
	ctx := context.Background()

	if err := cache.StorePasswordReset(ctx, "hash-1", 42, time.Minute); err != nil {
		t.Fatalf("store: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan int64, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := cache.ConsumePasswordReset(ctx, "hash-1"); err == nil {
				results <- id
			}
		}()
	}
	wg.Wait()
	close(results)
	var got []int64
	for id := range results {
		got = append(got, id)
	}
	if len(got) != 1 || got[0] != 42 {
		t.Fatalf("parallel consumes returned %v, want exactly one 42", got)
	}
	if _, err := cache.ConsumePasswordReset(ctx, "hash-1"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("third consume: want ErrNotFound, got %v", err)
	}
}

func TestTokenRedis_PasswordReset_NewTokenInvalidatesOld(t *testing.T) {
	cache := repository.NewTokenRedis(setupRedis(t))
	ctx := context.Background()

	cache.StorePasswordReset(ctx, "old", 7, time.Minute) //nolint:errcheck
	if err := cache.StorePasswordReset(ctx, "new", 7, time.Minute); err != nil {
		t.Fatalf("store new: %v", err)
	}
	if _, err := cache.ConsumePasswordReset(ctx, "old"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("old token: want ErrNotFound, got %v", err)
	}
	if id, err := cache.ConsumePasswordReset(ctx, "new"); err != nil || id != 7 {
		t.Errorf("new token: got (%d, %v), want (7, nil)", id, err)
	}
}

func TestTokenRedis_DeletePasswordReset(t *testing.T) {
	rdb := setupRedis(t)
	cache := repository.NewTokenRedis(rdb)
	ctx := context.Background()

	cache.StorePasswordReset(ctx, "h", 8, time.Minute) //nolint:errcheck
	if err := cache.DeletePasswordReset(ctx, 8); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := rdb.Exists(ctx, "prt-user:8").Val(); n != 0 {
		t.Error("prt-user:8 should be deleted")
	}
}

func TestTokenRedis_AcquireResetSlot_Cooldown(t *testing.T) {
	cache := repository.NewTokenRedis(setupRedis(t))
	ctx := context.Background()

	first, err := cache.AcquireResetSlot(ctx, "a@x.com", time.Minute, 24*time.Hour, 5)
	if err != nil || !first {
		t.Fatalf("first acquire: got (%v, %v), want (true, nil)", first, err)
	}
	second, err := cache.AcquireResetSlot(ctx, "a@x.com", time.Minute, 24*time.Hour, 5)
	if err != nil || second {
		t.Errorf("second acquire within the cooldown: got (%v, %v), want (false, nil)", second, err)
	}
	if other, _ := cache.AcquireResetSlot(ctx, "b@x.com", time.Minute, 24*time.Hour, 5); !other {
		t.Error("cooldown must be per email")
	}

	// A failed attempt gives the slot back, so the user can retry at once.
	if err := cache.ReleaseResetSlot(ctx, "a@x.com"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if again, _ := cache.AcquireResetSlot(ctx, "a@x.com", time.Minute, 24*time.Hour, 5); !again {
		t.Error("acquire after release: want true")
	}
}

func TestTokenRedis_AcquireResetSlot_DailyCap(t *testing.T) {
	cache := repository.NewTokenRedis(setupRedis(t))
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		ok, err := cache.AcquireResetSlot(ctx, "v@x.com", time.Millisecond, 24*time.Hour, 3)
		if err != nil || !ok {
			t.Fatalf("acquire %d: got (%v, %v), want (true, nil)", i, ok, err)
		}
		time.Sleep(5 * time.Millisecond) // let the 1 ms cooldown expire
	}
	if ok, _ := cache.AcquireResetSlot(ctx, "v@x.com", time.Millisecond, 24*time.Hour, 3); ok {
		t.Error("fourth acquire within the window: want false (daily cap)")
	}
}

func TestTokenRedis_RevokeAllRefreshTokens(t *testing.T) {
	rdb := setupRedis(t)
	cache := repository.NewTokenRedis(rdb)
	ctx := context.Background()

	for _, jti := range []string{"j1", "j2", "j3"} {
		if err := cache.StoreRefreshToken(ctx, jti, 99, time.Hour, time.Now()); err != nil {
			t.Fatalf("store %s: %v", jti, err)
		}
	}
	cache.StoreRefreshToken(ctx, "other-user", 100, time.Hour, time.Now()) //nolint:errcheck

	if err := cache.RevokeAllRefreshTokens(ctx, 99, time.Now(), time.Hour); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for _, jti := range []string{"j1", "j2", "j3"} {
		if _, err := cache.ConsumeRefreshToken(ctx, jti); !errors.Is(err, service.ErrNotFound) {
			t.Errorf("%s after revoke: want ErrNotFound, got %v", jti, err)
		}
	}
	if _, err := cache.ConsumeRefreshToken(ctx, "other-user"); err != nil {
		t.Errorf("another user's token must survive: %v", err)
	}
	if n := rdb.Exists(ctx, "urt:99").Val(); n != 0 {
		t.Error("urt:99 should be deleted after revoke")
	}
}

func TestTokenRedis_ConsumeRefreshToken_RemovesFromIndex(t *testing.T) {
	rdb := setupRedis(t)
	cache := repository.NewTokenRedis(rdb)
	ctx := context.Background()

	cache.StoreRefreshToken(ctx, "j1", 5, time.Hour, time.Now()) //nolint:errcheck
	cache.StoreRefreshToken(ctx, "j2", 5, time.Hour, time.Now()) //nolint:errcheck
	if _, err := cache.ConsumeRefreshToken(ctx, "j1"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	members := rdb.SMembers(ctx, "urt:5").Val()
	if len(members) != 1 || members[0] != "j2" {
		t.Errorf("urt:5 = %v, want [j2]", members)
	}
	if ttl := rdb.TTL(ctx, "urt:5").Val(); ttl <= 0 {
		t.Errorf("urt:5 must expire, TTL = %v", ttl)
	}
}

func TestUserMySQL_UpdatePasswordHash(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewUserMySQL(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &service.User{ID: 11, Email: "pw@x.com", PasswordHash: "old", Plan: "free"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.UpdatePasswordHash(ctx, 11, "new-hash"); err != nil {
		t.Fatalf("update: %v", err)
	}
	u, err := repo.GetByID(ctx, 11)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if u.PasswordHash != "new-hash" {
		t.Errorf("password_hash = %q, want new-hash", u.PasswordHash)
	}
}

func TestTokenRedis_StoreRefreshToken_BlockedAfterRevoke(t *testing.T) {
	cache := repository.NewTokenRedis(setupRedis(t))
	ctx := context.Background()

	// A refresh or login that was authorised before the revoke (by an old
	// refresh token or the old password) must not be able to store a token
	// after it: that is the race between a refresh and a password reset.
	authorisedBefore := time.Now()
	revokedAt := authorisedBefore.Add(10 * time.Millisecond)
	if err := cache.RevokeAllRefreshTokens(ctx, 31, revokedAt, time.Hour); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	err := cache.StoreRefreshToken(ctx, "late", 31, time.Hour, authorisedBefore)
	if !errors.Is(err, service.ErrInvalidToken) {
		t.Fatalf("store authorised before the revoke: want ErrInvalidToken, got %v", err)
	}
	if _, err := cache.ConsumeRefreshToken(ctx, "late"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("blocked token must not be stored, consume got %v", err)
	}

	if err := cache.StoreRefreshToken(ctx, "fresh", 31, time.Hour, revokedAt.Add(time.Millisecond)); err != nil {
		t.Errorf("store authorised after the revoke: %v", err)
	}
	if err := cache.StoreRefreshToken(ctx, "other", 32, time.Hour, authorisedBefore); err != nil {
		t.Errorf("another user's store must not be affected: %v", err)
	}
}

func TestLinkMySQL_QuotaUsage(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewLinkMySQL(db)
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash, quota_daily) VALUES (1, 'a@b.com', 'x', 5), (2, 'c@d.com', 'x', 100)`) //nolint:errcheck
	for _, l := range []*service.Link{
		{ID: 10, UserID: 1, ShortCode: "old", OriginalURL: "https://e.com"},   // before sinceID
		{ID: 20, UserID: 1, ShortCode: "new1", OriginalURL: "https://e.com"},  // counted
		{ID: 30, UserID: 1, ShortCode: "new2", OriginalURL: "https://e.com"},  // counted
		{ID: 40, UserID: 2, ShortCode: "other", OriginalURL: "https://e.com"}, // other user
	} {
		if err := repo.Create(ctx, l); err != nil {
			t.Fatalf("create %s: %v", l.ShortCode, err)
		}
	}

	used, limit, err := repo.QuotaUsage(ctx, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if used != 2 || limit != 5 {
		t.Errorf("want used=2 limit=5, got used=%d limit=%d", used, limit)
	}

	if _, _, err := repo.QuotaUsage(ctx, 999, 0); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("unknown user: want ErrNotFound, got %v", err)
	}
}
