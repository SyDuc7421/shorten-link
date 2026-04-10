package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/april/shorten-link/internal/service"
)

const keyPrefix = "sl:"

type linkRedis struct {
	rdb *redis.Client
}

// NewLinkRedis returns a LinkCache backed by Redis.
func NewLinkRedis(rdb *redis.Client) service.LinkCache {
	return &linkRedis{rdb: rdb}
}

func (c *linkRedis) Get(ctx context.Context, code string) (string, error) {
	val, err := c.rdb.Get(ctx, keyPrefix+code).Result()
	if errors.Is(err, redis.Nil) {
		return "", service.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("cache: get %q: %w", code, err)
	}
	return val, nil
}

func (c *linkRedis) Set(ctx context.Context, code string, url string, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, keyPrefix+code, url, ttl).Err(); err != nil {
		return fmt.Errorf("cache: set %q: %w", code, err)
	}
	return nil
}

func (c *linkRedis) Delete(ctx context.Context, code string) error {
	if err := c.rdb.Del(ctx, keyPrefix+code).Err(); err != nil {
		return fmt.Errorf("cache: delete %q: %w", code, err)
	}
	return nil
}
