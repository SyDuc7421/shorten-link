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

func (c *tokenRedis) StoreRefreshToken(ctx context.Context, tokenID string, userID int64, ttl time.Duration) error {
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, tokenPrefix+tokenID, userID, ttl)
		p.SAdd(ctx, userTokensKey(userID), tokenID)
		p.Expire(ctx, userTokensKey(userID), ttl)
		return nil
	})
	if err != nil {
		return fmt.Errorf("token cache: store: %w", err)
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

func (c *tokenRedis) RevokeAllRefreshTokens(ctx context.Context, userID int64) error {
	ids, err := c.rdb.SMembers(ctx, userTokensKey(userID)).Result()
	if err != nil {
		return fmt.Errorf("token cache: list user tokens: %w", err)
	}
	keys := []string{userTokensKey(userID)}
	for _, id := range ids {
		keys = append(keys, tokenPrefix+id)
	}
	if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("token cache: revoke user tokens: %w", err)
	}
	return nil
}

const (
	resetPrefix         = "prt:"
	resetUserPrefix     = "prt-user:"
	resetCooldownPrefix = "prt-cooldown:"
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

func (c *tokenRedis) AcquireResetCooldown(ctx context.Context, email string, ttl time.Duration) (bool, error) {
	err := c.rdb.SetArgs(ctx, resetCooldownPrefix+email, 1, redis.SetArgs{Mode: "NX", TTL: ttl}).Err()
	if errors.Is(err, redis.Nil) { // key exists: still cooling down
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("token cache: reset cooldown: %w", err)
	}
	return true, nil
}
