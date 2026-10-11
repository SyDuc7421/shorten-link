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

func TestRateLimit_IgnoresSpoofedXForwardedFor(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-xff", 1, time.Minute)(okHandler())

	for i, xff := range []string{"1.1.1.1", "2.2.2.2"} {
		req := requestFrom("203.0.113.5")
		req.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		limiter.ServeHTTP(w, req)
		if i == 1 && w.Code != http.StatusTooManyRequests {
			t.Errorf("spoofed XFF bypassed limit: got %d", w.Code)
		}
	}
}

func TestRateLimit_UsesXRealIPFromTrustedProxy(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-realip", 1, time.Minute)(okHandler())

	for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		req := requestFrom("172.18.0.2") // nginx on the private network
		req.Header.Set("X-Real-IP", ip)
		w := httptest.NewRecorder()
		limiter.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("client %s behind proxy: want 200, got %d", ip, w.Code)
		}
	}
}

func TestRateLimit_IgnoresXRealIPFromPublicPeer(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-realip-pub", 1, time.Minute)(okHandler())

	for i, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		req := requestFrom("203.0.113.9")
		req.Header.Set("X-Real-IP", ip)
		w := httptest.NewRecorder()
		limiter.ServeHTTP(w, req)
		if i == 1 && w.Code != http.StatusTooManyRequests {
			t.Errorf("spoofed X-Real-IP bypassed limit: got %d", w.Code)
		}
	}
}

func TestRateLimit_RejectionHeaders(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-headers", 1, 30*time.Second)(okHandler())

	limiter.ServeHTTP(httptest.NewRecorder(), requestFrom("10.0.0.9"))
	w := httptest.NewRecorder()
	limiter.ServeHTTP(w, requestFrom("10.0.0.9"))

	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After: want 30, got %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", got)
	}
}

func TestRateLimit_FailsOpenWhenRedisDown(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { rdb.Close() })
	limiter := middleware.RateLimit(rdb, "test-down", 1, time.Minute)(okHandler())

	w := httptest.NewRecorder()
	limiter.ServeHTTP(w, requestFrom("10.0.0.10"))
	if w.Code != http.StatusOK {
		t.Errorf("want 200 when Redis unavailable, got %d", w.Code)
	}
}
