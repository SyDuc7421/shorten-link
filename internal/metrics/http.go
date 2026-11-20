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

// RecordRoute sits just inside TimeoutHandler. It reads the pattern mux would
// match into the holder, then calls next (the rest of the chain, ending in mux).
// The holder is filled before anything else runs, so requests cut off by
// TimeoutHandler or rejected before the mux (429, 413, CORS) still have their
// route. mux is only asked for the pattern, never served directly, so path
// values are populated by the mux inside next.
func RecordRoute(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CONNECT is skipped: for a trailing-slash redirect ServeMux returns the
		// raw request path as the "pattern", which would be a free-form label.
		if h, ok := r.Context().Value(holderKey{}).(*routeHolder); ok && r.Method != http.MethodConnect {
			if _, pattern := mux.Handler(r); pattern != "" {
				h.pattern.Store(&pattern)
			}
		}
		next.ServeHTTP(w, r)
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
