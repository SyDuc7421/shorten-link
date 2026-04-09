// Package ctxkey defines typed context keys shared across middleware and handlers.
package ctxkey

import "context"

type userIDKey struct{}

// SetUserID stores a userID in ctx. Called by auth middleware.
func SetUserID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, userIDKey{}, id)
}

// UserID retrieves the authenticated user's ID. ok is false if not set.
func UserID(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(userIDKey{}).(int64)
	return id, ok
}
