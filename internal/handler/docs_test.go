package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/april/shorten-link/internal/handler"
	"github.com/april/shorten-link/internal/middleware"
)

func TestDocs_CSPAllowsSwaggerAssets(t *testing.T) {
	mux := http.NewServeMux()
	handler.RegisterDocs(mux)

	w := httptest.NewRecorder()
	middleware.SecureHeaders(mux).ServeHTTP(w, httptest.NewRequest("GET", "/docs", nil))

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src https://unpkg.com 'sha256-", "style-src https://unpkg.com", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	if strings.Contains(w.Body.String(), "swagger-ui-dist@5/") {
		t.Error("swagger-ui-dist version is not pinned")
	}
}
