package repository_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
