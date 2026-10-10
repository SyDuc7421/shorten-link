package middleware

import (
	"net/http"
	"strings"

	"github.com/april/shorten-link/internal/ctxkey"
)

// TokenParser validates an access token and returns the userID.
// Implemented by *service.AuthService.
type TokenParser interface {
	ParseAccessToken(token string) (int64, error)
}

// JWTAuth extracts and validates the Bearer token, then stores the userID in context.
// Must be applied before RequireAuth on protected routes.
func JWTAuth(parser TokenParser) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if strings.HasPrefix(header, "Bearer ") {
				token := strings.TrimPrefix(header, "Bearer ")
				if userID, err := parser.ParseAccessToken(token); err == nil {
					r = r.WithContext(ctxkey.SetUserID(r.Context(), userID))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuth returns 401 if no userID is present in the context.
// Apply JWTAuth upstream to populate the userID.
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

// TokenParserFunc lets a plain function satisfy TokenParser.
type TokenParserFunc func(token string) (int64, error)

func (f TokenParserFunc) ParseAccessToken(token string) (int64, error) {
	return f(token)
}
