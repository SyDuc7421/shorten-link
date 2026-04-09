package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/logger"
)

func RequestID(base *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" {
				requestID = generateID()
			}
			w.Header().Set("X-Request-ID", requestID)

			l := base.With(zap.String("request_id", requestID))
			ctx := logger.WithContext(r.Context(), l)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
