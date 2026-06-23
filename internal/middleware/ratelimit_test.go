package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/april/shorten-link/internal/middleware"
)

func setupTestRedis(t *testing.T) *redis.Client {
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
	addr = strings.TrimPrefix(addr, "redis://")

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func requestFrom(ip string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = ip + ":1234"
	return req
}

func TestRateLimit_AllowsWithinLimit(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-allow", 3, time.Minute)(okHandler())

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		limiter.ServeHTTP(w, requestFrom("10.0.0.1"))
		if w.Code != http.StatusOK {
			t.Errorf("request %d: want 200, got %d", i+1, w.Code)
		}
	}
}

func TestRateLimit_BlocksOverLimit(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-block", 2, time.Minute)(okHandler())

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		limiter.ServeHTTP(w, requestFrom("10.0.0.2"))
		if w.Code != http.StatusOK {
			t.Errorf("request %d: want 200, got %d", i+1, w.Code)
		}
	}

	w := httptest.NewRecorder()
	limiter.ServeHTTP(w, requestFrom("10.0.0.2"))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("over-limit request: want 429, got %d", w.Code)
	}
}

func TestRateLimit_SeparatePerIP(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-perip", 1, time.Minute)(okHandler())

	w1 := httptest.NewRecorder()
	limiter.ServeHTTP(w1, requestFrom("10.0.0.3"))
	if w1.Code != http.StatusOK {
		t.Errorf("IP A first request: want 200, got %d", w1.Code)
	}

	w2 := httptest.NewRecorder()
	limiter.ServeHTTP(w2, requestFrom("10.0.0.3"))
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("IP A second request: want 429, got %d", w2.Code)
	}

	w3 := httptest.NewRecorder()
	limiter.ServeHTTP(w3, requestFrom("10.0.0.4"))
	if w3.Code != http.StatusOK {
		t.Errorf("IP B first request: want 200, got %d (should not share limit with IP A)", w3.Code)
	}
}
