package metrics

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

type holderKey struct{}

// routeHolder carries the matched ServeMux pattern from the inner handler back
// to Middleware. Under http.TimeoutHandler the inner handler runs in another
// goroutine, so access is atomic.
type routeHolder struct{ pattern atomic.Pointer[string] }

// statusWriter records the status code. It embeds the ResponseWriter, so
// Unwrap lets http.ResponseController reach the real one.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Middleware is the outermost HTTP wrapper. It counts requests and times them
// by method, route pattern and status. The route comes from RecordRoute; a
// request no pattern matched is labelled "unmatched".
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		holder := &routeHolder{}
		r = r.WithContext(context.WithValue(r.Context(), holderKey{}, holder))
		sw := &statusWriter{ResponseWriter: w}

		HTTPInFlight.Inc()
		start := time.Now()
		defer func() {
			HTTPInFlight.Dec()
			method := methodLabel(r.Method)
			route := "unmatched"
			if p := holder.pattern.Load(); p != nil {
				route = *p
			}
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			HTTPRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
			HTTPDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		}()
		next.ServeHTTP(sw, r)
	})
}

// RecordRoute is the innermost wrapper around the mux. It reads the matched
// pattern into the holder before the handler runs, so a request cut off by
// TimeoutHandler still has its route. It then serves through mux.ServeHTTP
// (not the handler mux.Handler returns) so path values are populated.
func RecordRoute(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := r.Context().Value(holderKey{}).(*routeHolder); ok {
			if _, pattern := mux.Handler(r); pattern != "" {
				h.pattern.Store(&pattern)
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return m
	}
	return "OTHER"
}
