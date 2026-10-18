package middleware

import (
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/logger"
)

// AccessLog writes one "request" log line per request after it finishes, with
// method, path (no query string, which may carry user input), status, bytes,
// duration, client IP and user ID when authenticated. Must run inside
// RequestID so the line carries request_id.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		fields := []zap.Field{
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rec.statusOrOK()),
			zap.Int("bytes", rec.bytes),
			zap.Duration("duration", time.Since(start)),
			zap.String("ip", clientIP(r)),
		}
		if userID, ok := ctxkey.UserID(r.Context()); ok {
			fields = append(fields, zap.Int64("user_id", userID))
		}
		logger.FromContext(r.Context()).Info("request", fields...)
	})
}

// statusRecorder remembers the status code and body size a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) statusOrOK() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
