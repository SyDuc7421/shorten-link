package logger

import (
	"context"

	"go.uber.org/zap"
)

type contextKey struct{}

func New() (*zap.Logger, error) {
	return zap.NewProduction()
}

func WithContext(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

func FromContext(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(contextKey{}).(*zap.Logger); ok {
		return l
	}
	return zap.L()
}
