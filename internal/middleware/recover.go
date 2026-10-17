package middleware

import (
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/logger"
)

// Recover turns a handler panic into a logged error and a JSON 500, instead of
// net/http's default of dropping the connection. If the handler already started
// the response, the status cannot change, so only the log is written.
// http.ErrAbortHandler is re-panicked: it is net/http's deliberate way to abort.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { //nolint:errorlint // sentinel compared as net/http does
				panic(p)
			}
			logger.FromContext(r.Context()).Error("panic recovered",
				zap.String("panic", fmt.Sprint(p)),
				zap.Stack("stack"),
			)
			if rec.status == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal server error"}` + "\n"))
			}
		}()
		next.ServeHTTP(rec, r)
	})
}
