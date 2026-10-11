package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
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
	retryAfter := strconv.Itoa(int(window.Seconds()))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			key := fmt.Sprintf("rl:%s:%s", action, ip)
			now := time.Now().UnixMilli()
			member := uniqueMember()

			allowed, err := slidingWindowScript.Run(
				r.Context(), rdb,
				[]string{key},
				now, windowMS, limit, member,
			).Int()
			// Fail open: a Redis outage must not take down redirects.
			if err == nil && allowed == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", retryAfter)
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"rate limit exceeded"}`)) //nolint:errcheck
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// clientIP returns the client address. X-Real-IP (set by nginx from
// $remote_addr) is trusted only when the direct peer is a private or loopback
// address, i.e. our own proxy. X-Forwarded-For is never trusted because nginx
// appends to whatever the client sent.
func clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if ip := net.ParseIP(peer); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
		if real := net.ParseIP(r.Header.Get("X-Real-IP")); real != nil {
			return real.String()
		}
	}
	return peer
}

func uniqueMember() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
