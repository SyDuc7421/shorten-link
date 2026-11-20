package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/logger"
)

// validRequestID is the only shape of client-supplied ID we accept; anything else
// could inject into logs or AMQP headers.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func RequestID(base *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-ID")
			if !validRequestID.MatchString(requestID) {
				requestID = generateID()
			}
			w.Header().Set("X-Request-ID", requestID)

			l := base.With(zap.String("request_id", requestID))
			ctx := logger.WithContext(r.Context(), l)
			ctx = ctxkey.SetRequestID(ctx, requestID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
