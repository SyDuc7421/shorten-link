package middleware

import (
	"net/http"

	"github.com/april/shorten-link/internal/ctxkey"
)

// RequireAuth returns 401 if no userID is present in the context.
// The actual JWT validation that populates userID is added in Phase 5.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ctxkey.UserID(r.Context()); !ok {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
