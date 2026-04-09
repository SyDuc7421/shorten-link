package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowScript atomically enforces a sliding window rate limit.
// Returns 1 if the request is allowed, 0 if rate limited.
var slidingWindowScript = redis.NewScript(`
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local member = ARGV[4]
redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)
local count = redis.call('ZCARD', key)
if count >= limit then
  return 0
end
redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, window + 1000)
return 1
`)

// RateLimit returns a middleware that limits requests using a Redis sliding window.
// action is a label used in the Redis key (e.g. "redirect", "create").
// limit is the max requests per window duration.
func RateLimit(rdb *redis.Client, action string, limit int, window time.Duration) func(http.Handler) http.Handler {
	windowMS := window.Milliseconds()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			key := fmt.Sprintf("rl:%s:%s", action, ip)
			now := time.Now().UnixMilli()
			member := uniqueMember()

			allowed, err := slidingWindowScript.Run(
				context.Background(), rdb,
				[]string{key},
				now, windowMS, limit, member,
			).Int()
			if err != nil || allowed == 0 {
				w.Header().Set("Retry-After", "60")
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// clientIP extracts the real client IP, preferring X-Forwarded-For.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip, _, err := net.SplitHostPort(xff); err == nil {
			return ip
		}
		return xff
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func uniqueMember() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
