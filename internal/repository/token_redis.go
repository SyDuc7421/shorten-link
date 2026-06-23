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

func (c *tokenRedis) GetRefreshToken(ctx context.Context, tokenID string) (int64, error) {
	val, err := c.rdb.Get(ctx, tokenPrefix+tokenID).Result()
	if errors.Is(err, redis.Nil) {
		return 0, service.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("token cache: get: %w", err)
	}
	id, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("token cache: parse userID: %w", err)
	}
	return id, nil
}

func (c *tokenRedis) DeleteRefreshToken(ctx context.Context, tokenID string) error {
	if err := c.rdb.Del(ctx, tokenPrefix+tokenID).Err(); err != nil {
		return fmt.Errorf("token cache: delete: %w", err)
	}
	return nil
}

const lockoutPrefix = "lockout:"

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
