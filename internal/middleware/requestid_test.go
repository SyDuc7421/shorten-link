package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/ctxkey"
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

func TestRequestID_RejectsInvalidHeader(t *testing.T) {
	cases := map[string]string{
		"too long": strings.Repeat("a", 65),
		"newline":  "a\nb",
		"unicode":  "ä",
		"space":    "a b",
		"empty":    "",
	}
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			h := middleware.RequestID(zap.NewNop())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
			req := httptest.NewRequest("GET", "/", nil)
			req.Header["X-Request-Id"] = []string{in}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if got := w.Header().Get("X-Request-ID"); got == in || !hex32.MatchString(got) {
				t.Errorf("want fresh 32-hex id, got %q", got)
			}
		})
	}
	h := middleware.RequestID(zap.NewNop())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "abc_DEF-123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("X-Request-ID"); got != "abc_DEF-123" {
		t.Errorf("valid id replaced: %q", got)
	}
}

func TestRequestID_StoredInContext(t *testing.T) {
	var got string
	var ok bool
	h := middleware.RequestID(zap.NewNop())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = ctxkey.RequestID(r.Context())
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if !ok || got == "" || got != w.Header().Get("X-Request-ID") {
		t.Errorf("ctx id %q (ok=%v) != header %q", got, ok, w.Header().Get("X-Request-ID"))
	}
}
