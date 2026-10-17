package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/april/shorten-link/internal/middleware"
)

func TestCORS_PreflightAllowsEveryMethodTheAPIUses(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	h := middleware.CORS([]string{"http://localhost:3000"})(next)

	req := httptest.NewRequest(http.MethodOptions, "/api/admin/users/1", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "PATCH")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	allowed := w.Header().Get("Access-Control-Allow-Methods")
	for _, m := range []string{"GET", "POST", "PATCH", "DELETE"} {
		if !strings.Contains(allowed, m) {
			t.Errorf("Access-Control-Allow-Methods %q lacks %s", allowed, m)
		}
	}
}
