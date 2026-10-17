package middleware

import (
	"context"
	"encoding/json"
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

// AdminChecker reports whether a user is an enabled admin.
// Implemented by *service.AdminService.
type AdminChecker interface {
	IsActiveAdmin(ctx context.Context, userID int64) (bool, error)
}

// RequireAdmin lets the request through only for an enabled admin: 401 without
// a user, 403 for anyone else, 500 if the check itself fails. It asks the
// checker on every request, so revoking an admin does not wait for their
// token to expire. Apply after RequireAuth.
func RequireAdmin(checker AdminChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := ctxkey.UserID(r.Context())
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			isAdmin, err := checker.IsActiveAdmin(r.Context(), userID)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "failed to check permissions")
				return
			}
			if !isAdmin {
				writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}

// TokenParserFunc lets a plain function satisfy TokenParser.
type TokenParserFunc func(token string) (int64, error)

func (f TokenParserFunc) ParseAccessToken(token string) (int64, error) {
	return f(token)
}
