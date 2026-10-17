package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/april/shorten-link/internal/service"
)

const tokenPrefix = "rt:"

type tokenRedis struct {
	rdb *redis.Client
}

// NewTokenRedis returns a TokenCache backed by Redis.
func NewTokenRedis(rdb *redis.Client) service.TokenCache {
	return &tokenRedis{rdb: rdb}
}

// userTokensKey holds the set of refresh token IDs issued to a user, so all of
// them can be revoked at once. Entries for tokens that expired on their own may
// linger until the set itself expires; they point at keys that no longer exist.
func userTokensKey(userID int64) string { return "urt:" + strconv.FormatInt(userID, 10) }

// userRevokedKey holds the time (Unix ms) of the user's last "revoke all".
func userRevokedKey(userID int64) string { return "urv:" + strconv.FormatInt(userID, 10) }

// storeRefreshScript stores a refresh token unless the user's sessions were
// revoked at or after the moment the request was authorised. Checking and
// storing in one script closes the race where a refresh consumes its old token,
// a password reset revokes everything, and the refresh then stores a new token.
var storeRefreshScript = redis.NewScript(`
local revoked = redis.call('GET', KEYS[3])
if revoked and tonumber(revoked) >= tonumber(ARGV[3]) then
  return 0
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('SADD', KEYS[2], ARGV[4])
redis.call('PEXPIRE', KEYS[2], ARGV[2])
return 1
`)

func (c *tokenRedis) StoreRefreshToken(ctx context.Context, tokenID string, userID int64, ttl time.Duration, authorizedAt time.Time) error {
	stored, err := storeRefreshScript.Run(ctx, c.rdb,
		[]string{tokenPrefix + tokenID, userTokensKey(userID), userRevokedKey(userID)},
		userID, ttl.Milliseconds(), authorizedAt.UnixMilli(), tokenID,
	).Int()
	if err != nil {
		return fmt.Errorf("token cache: store: %w", err)
	}
	if stored == 0 {
		return service.ErrInvalidToken
	}
	return nil
}

func (c *tokenRedis) ConsumeRefreshToken(ctx context.Context, tokenID string) (int64, error) {
	val, err := c.rdb.GetDel(ctx, tokenPrefix+tokenID).Result()
	if errors.Is(err, redis.Nil) {
		return 0, service.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("token cache: consume: %w", err)
	}
	id, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("token cache: parse userID: %w", err)
	}
	// The token is already consumed; a stale index entry is harmless.
	c.rdb.SRem(ctx, userTokensKey(id), tokenID) //nolint:errcheck
	return id, nil
}

const lockoutPrefix = "lockout:"

func (c *tokenRedis) GetLockout(ctx context.Context, email string) (int64, error) {
	n, err := c.rdb.Get(ctx, lockoutPrefix+email).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("token cache: get lockout: %w", err)
	}
	return n, nil
}

func (c *tokenRedis) IncrLockout(ctx context.Context, email string, ttl time.Duration) (int64, error) {
	key := lockoutPrefix + email
	count, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("token cache: incr lockout: %w", err)
	}
	// Set TTL only on first increment so the window starts fresh.
	if count == 1 {
		c.rdb.Expire(ctx, key, ttl) //nolint:errcheck
	}
	return count, nil
}

func (c *tokenRedis) DeleteLockout(ctx context.Context, email string) error {
	c.rdb.Del(ctx, lockoutPrefix+email) //nolint:errcheck
	return nil
}

// revokeAllScript records the revoke time and deletes every indexed token in
// one atomic step, so no store can slip in between reading and deleting the set.
// Token keys are built from ARGV[3], which is fine on a single Redis instance
// but not on Redis Cluster.
var revokeAllScript = redis.NewScript(`
redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[2])
local ids = redis.call('SMEMBERS', KEYS[1])
for _, id in ipairs(ids) do
  redis.call('DEL', ARGV[3] .. id)
end
redis.call('DEL', KEYS[1])
return #ids
`)

func (c *tokenRedis) RevokeAllRefreshTokens(ctx context.Context, userID int64, revokedAt time.Time, ttl time.Duration) error {
	err := revokeAllScript.Run(ctx, c.rdb,
		[]string{userTokensKey(userID), userRevokedKey(userID)},
		revokedAt.UnixMilli(), ttl.Milliseconds(), tokenPrefix,
	).Err()
	if err != nil {
		return fmt.Errorf("token cache: revoke user tokens: %w", err)
	}
	return nil
}

const (
	resetPrefix         = "prt:"
	resetUserPrefix     = "prt-user:"
	resetCooldownPrefix = "prt-cooldown:"
	resetDailyPrefix    = "prt-daily:"
)

func (c *tokenRedis) StorePasswordReset(ctx context.Context, tokenHash string, userID int64, ttl time.Duration) error {
	userKey := resetUserPrefix + strconv.FormatInt(userID, 10)
	old, err := c.rdb.Get(ctx, userKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("token cache: get previous reset: %w", err)
	}
	_, err = c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		if old != "" {
			p.Del(ctx, resetPrefix+old)
		}
		p.Set(ctx, resetPrefix+tokenHash, userID, ttl)
		p.Set(ctx, userKey, tokenHash, ttl)
		return nil
	})
	if err != nil {
		return fmt.Errorf("token cache: store reset: %w", err)
	}
	return nil
}

func (c *tokenRedis) ConsumePasswordReset(ctx context.Context, tokenHash string) (int64, error) {
	val, err := c.rdb.GetDel(ctx, resetPrefix+tokenHash).Result()
	if errors.Is(err, redis.Nil) {
		return 0, service.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("token cache: consume reset: %w", err)
	}
	id, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("token cache: parse reset userID: %w", err)
	}
	return id, nil
}

func (c *tokenRedis) DeletePasswordReset(ctx context.Context, userID int64) error {
	if err := c.rdb.Del(ctx, resetUserPrefix+strconv.FormatInt(userID, 10)).Err(); err != nil {
		return fmt.Errorf("token cache: delete reset: %w", err)
	}
	return nil
}

// acquireResetSlotScript allows a reset mail unless one went out within the
// cooldown or the daily cap for the address is reached.
var acquireResetSlotScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
local n = redis.call('INCR', KEYS[2])
if n == 1 then
  redis.call('PEXPIRE', KEYS[2], ARGV[2])
end
if n > tonumber(ARGV[3]) then
  return 0
end
redis.call('SET', KEYS[1], 1, 'PX', ARGV[1])
return 1
`)

func (c *tokenRedis) AcquireResetSlot(ctx context.Context, email string, cooldown, window time.Duration, max int) (bool, error) {
	ok, err := acquireResetSlotScript.Run(ctx, c.rdb,
		[]string{resetCooldownPrefix + email, resetDailyPrefix + email},
		cooldown.Milliseconds(), window.Milliseconds(), max,
	).Int()
	if err != nil {
		return false, fmt.Errorf("token cache: reset slot: %w", err)
	}
	return ok == 1, nil
}

func (c *tokenRedis) ReleaseResetSlot(ctx context.Context, email string) error {
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, resetCooldownPrefix+email)
		p.Decr(ctx, resetDailyPrefix+email)
		return nil
	})
	if err != nil {
		return fmt.Errorf("token cache: release reset slot: %w", err)
	}
	return nil
}
