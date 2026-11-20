package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/april/shorten-link/internal/metrics"
	"github.com/april/shorten-link/internal/middleware"
)

func count(method, route string, status int) float64 {
	return testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues(method, route, strconv.Itoa(status)))
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{code}", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("POST /api/links", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("created"))
	})
	return mux
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestMiddleware_RouteIsPatternNotPath(t *testing.T) {
	h := metrics.Middleware(metrics.RecordRoute(newMux(), newMux()))
	before := count("GET", "GET /{code}", 404)
	series := testutil.CollectAndCount(metrics.HTTPRequests)

	do(h, "GET", "/abc")
	do(h, "GET", "/xyz")

	if d := count("GET", "GET /{code}", 404) - before; d != 2 {
		t.Fatalf("delta = %v, want 2", d)
	}
	if grew := testutil.CollectAndCount(metrics.HTTPRequests) - series; grew > 1 {
		t.Fatalf("series grew by %d, want at most 1", grew)
	}
}

func TestMiddleware_Unmatched(t *testing.T) {
	h := metrics.Middleware(metrics.RecordRoute(newMux(), newMux()))
	b404 := count("DELETE", "unmatched", 404)
	b405 := count("DELETE", "unmatched", 405)
	rec := do(h, "DELETE", "/nope/deep")
	if rec.Code != 404 && rec.Code != 405 {
		t.Fatalf("status = %d", rec.Code)
	}
	got := (count("DELETE", "unmatched", 404) - b404) + (count("DELETE", "unmatched", 405) - b405)
	if got != 1 {
		t.Fatalf("unmatched delta = %v, want 1", got)
	}
}

func TestMiddleware_TimeoutCountedAs503(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{code}", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	})
	h := metrics.Middleware(http.TimeoutHandler(metrics.RecordRoute(mux, mux), 50*time.Millisecond, "x"))
	before := count("GET", "GET /{code}", 503)
	do(h, "GET", "/slow")
	if d := count("GET", "GET /{code}", 503) - before; d != 1 {
		t.Fatalf("delta = %v, want 1", d)
	}
	time.Sleep(200 * time.Millisecond) // let the abandoned handler finish (race check)
}

func TestMiddleware_RequestThroughWithContext(t *testing.T) {
	type key struct{}
	mux := newMux()
	inner := metrics.RecordRoute(mux, mux)
	ctxMW := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), key{}, 1)))
	})
	h := metrics.Middleware(ctxMW)
	before := count("GET", "GET /{code}", 404)
	do(h, "GET", "/abc")
	if d := count("GET", "GET /{code}", 404) - before; d != 1 {
		t.Fatalf("delta = %v, want 1", d)
	}
}

func TestMiddleware_DefaultStatus200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})
	h := metrics.Middleware(metrics.RecordRoute(mux, mux))
	before := count("GET", "GET /ok", 200)
	do(h, "GET", "/ok")
	if d := count("GET", "GET /ok", 200) - before; d != 1 {
		t.Fatalf("delta = %v, want 1", d)
	}
}

func TestMiddleware_InFlightReturnsToZero(t *testing.T) {
	h := metrics.Middleware(metrics.RecordRoute(newMux(), newMux()))
	before := testutil.ToFloat64(metrics.HTTPInFlight)
	do(h, "GET", "/abc")
	if got := testutil.ToFloat64(metrics.HTTPInFlight); got != before {
		t.Fatalf("in flight = %v, want %v", got, before)
	}
}

func TestMiddleware_UnknownMethodIsOther(t *testing.T) {
	h := metrics.Middleware(metrics.RecordRoute(newMux(), newMux()))
	before404 := count("OTHER", "unmatched", 404)
	before405 := count("OTHER", "unmatched", 405)
	do(h, "BREW", "/abc")
	got := (count("OTHER", "unmatched", 404) - before404) + (count("OTHER", "unmatched", 405) - before405)
	if got != 1 {
		t.Fatalf("OTHER delta = %v, want 1", got)
	}
}

func TestRecordRoute_PathValueStillWorks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{code}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.PathValue("code")))
	})
	rec := do(metrics.Middleware(metrics.RecordRoute(mux, mux)), "GET", "/abc")
	if rec.Body.String() != "abc" {
		t.Fatalf("body = %q, want abc", rec.Body.String())
	}
}

func TestMiddleware_RateLimited429HasRoute(t *testing.T) {
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // like RateLimit: never reaches the mux
	})
	h := metrics.Middleware(metrics.RecordRoute(newMux(), limited))
	before := count("GET", "GET /{code}", 429)
	do(h, "GET", "/abc")
	if d := count("GET", "GET /{code}", 429) - before; d != 1 {
		t.Fatalf("delta = %v, want 1", d)
	}
}

func TestMiddleware_PanicCountedAs500(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{code}", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := metrics.Middleware(metrics.RecordRoute(mux, middleware.Recover(mux)))
	before := count("GET", "GET /{code}", 500)
	do(h, "GET", "/abc")
	if d := count("GET", "GET /{code}", 500) - before; d != 1 {
		t.Fatalf("delta = %v, want 1", d)
	}
}

// CONNECT to a subtree path without its trailing slash makes ServeMux return
// the raw path as the pattern; it must be counted as "unmatched", not as a new label.
func TestMiddleware_ConnectIsUnmatched(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/files/", func(http.ResponseWriter, *http.Request) {})
	h := metrics.Middleware(metrics.RecordRoute(mux, mux))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodConnect, "/files", nil))

	if got := count("OTHER", "unmatched", rec.Code); got < 1 {
		t.Fatalf("CONNECT /files: unmatched count = %v (status %d), want >= 1", got, rec.Code)
	}
	if got := count("OTHER", "/files", rec.Code); got != 0 {
		t.Fatalf("raw path leaked into the route label: %v", got)
	}
}
