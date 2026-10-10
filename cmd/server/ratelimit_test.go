package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

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
	rdb := redis.NewClient(&redis.Options{Addr: strings.TrimPrefix(addr, "redis://")})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// countUntilLimited sends requests to path from one IP and returns how many got
// through before the first 429 (or n if none was limited).
func countUntilLimited(h http.Handler, method, path string, n int) int {
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "203.0.113.7:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			return i
		}
	}
	return n
}

func TestWithRateLimits(t *testing.T) {
	rdb := setupRedis(t)
	h := withRateLimits(http.NotFoundHandler(), rdb)

	for _, tc := range []struct {
		method, path string
		limit        int
	}{
		{http.MethodPost, "/api/auth/login", 10},
		{http.MethodPost, "/api/auth/register", 5},
		{http.MethodPost, "/api/auth/refresh", 30},
		{http.MethodPost, "/api/links", 10},
		{http.MethodGet, "/abc123", 60},
	} {
		if got := countUntilLimited(h, tc.method, tc.path, tc.limit+5); got != tc.limit {
			t.Errorf("%s %s: want limited after %d requests, got %d", tc.method, tc.path, tc.limit, got)
		}
	}
	if got := countUntilLimited(h, http.MethodGet, "/healthz", 100); got != 100 {
		t.Errorf("/healthz must not be rate limited, got 429 after %d", got)
	}
}
