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

func (c *tokenRedis) StoreRefreshToken(ctx context.Context, tokenID string, userID int64, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, tokenPrefix+tokenID, userID, ttl).Err(); err != nil {
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
