package service

import (
	"context"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/logger"
)

// publishAsync publishes in a goroutine with a fresh publishTimeout context and
// logs a warning (with the routing key) on failure, using logger.FromContext(ctx).
// The logger is taken before the goroutine starts because ctx may be done by then.
func publishAsync(ctx context.Context, pub Publisher, routingKey string, payload any) {
	log := logger.FromContext(ctx)
	go func() {
		pctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		if err := pub.Publish(pctx, routingKey, payload); err != nil {
			log.Warn("publish failed", zap.String("routing_key", routingKey), zap.Error(err))
		}
	}()
}
