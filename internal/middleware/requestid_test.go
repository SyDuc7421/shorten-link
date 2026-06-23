package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/middleware"
)

func TestRequestID_PropagatesExistingHeader(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	h := middleware.RequestID(zap.NewNop())(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "my-fixed-id")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-ID"); got != "my-fixed-id" {
		t.Errorf("want X-Request-ID=my-fixed-id, got %q", got)
	}
}

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	h := middleware.RequestID(zap.NewNop())(next)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	id := w.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("expected X-Request-ID to be set")
	}
	if len(id) != 32 {
		t.Errorf("expected 32-char hex ID, got %q (len %d)", id, len(id))
	}
}

func TestRequestID_UniquePerRequest(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	h := middleware.RequestID(zap.NewNop())(next)

	w1, w2 := httptest.NewRecorder(), httptest.NewRecorder()
	h.ServeHTTP(w1, httptest.NewRequest("GET", "/", nil))
	h.ServeHTTP(w2, httptest.NewRequest("GET", "/", nil))

	id1 := w1.Header().Get("X-Request-ID")
	id2 := w2.Header().Get("X-Request-ID")
	if id1 == id2 {
		t.Errorf("expected unique IDs per request, both got %q", id1)
	}
}
