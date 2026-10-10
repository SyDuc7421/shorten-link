package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/handler"
	"github.com/april/shorten-link/internal/middleware"
	"github.com/april/shorten-link/internal/service"
)

// mockLinkSvc implements handler.LinkServicer.
type mockLinkSvc struct {
	shortenFn    func(ctx context.Context, userID int64, rawURL string, expiresAt *time.Time) (*service.Link, error)
	resolveFn    func(ctx context.Context, code string) (string, error)
	deleteFn     func(ctx context.Context, code string, userID int64) error
	listByUserFn func(ctx context.Context, userID int64, afterID int64, limit int) ([]*service.Link, error)
}

func (m *mockLinkSvc) Shorten(ctx context.Context, userID int64, rawURL string, expiresAt *time.Time) (*service.Link, error) {
	return m.shortenFn(ctx, userID, rawURL, expiresAt)
}
func (m *mockLinkSvc) Resolve(ctx context.Context, code string) (string, error) {
	return m.resolveFn(ctx, code)
}
func (m *mockLinkSvc) Delete(ctx context.Context, code string, userID int64) error {
	return m.deleteFn(ctx, code, userID)
}
func (m *mockLinkSvc) ListByUser(ctx context.Context, userID int64, afterID int64, limit int) ([]*service.Link, error) {
	return m.listByUserFn(ctx, userID, afterID, limit)
}

// withUserID adds an authenticated userID to the request context.
func withUserID(r *http.Request, id int64) *http.Request {
	return r.WithContext(ctxkey.SetUserID(r.Context(), id))
}

// newLinkMux builds a ServeMux with LinkHandler registered and a no-op auth wrapper.
func newLinkMux(svc handler.LinkServicer) *http.ServeMux {
	mux := http.NewServeMux()
	h := handler.NewLinkHandler(svc, "https://short.ly")
	h.Register(mux, func(next http.Handler) http.Handler { return next })
	return mux
}

// keep errors import used
var _ = errors.New

// --- Shorten tests ---

func TestLinkHandler_Shorten_OK(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{
		shortenFn: func(_ context.Context, _ int64, _ string, _ *time.Time) (*service.Link, error) {
			return &service.Link{ShortCode: "abc123", OriginalURL: "https://example.com"}, nil
		},
	})

	req := withUserID(
		httptest.NewRequest("POST", "/api/links", bytes.NewBufferString(`{"url":"https://example.com"}`)),
		1,
	)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("want 201, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp["code"] != "abc123" {
		t.Errorf("want code 'abc123', got %v", resp["code"])
	}
	if resp["short_url"] != "https://short.ly/abc123" {
		t.Errorf("want short_url 'https://short.ly/abc123', got %v", resp["short_url"])
	}
}

func TestLinkHandler_Shorten_NoAuth(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{})

	req := httptest.NewRequest("POST", "/api/links", bytes.NewBufferString(`{"url":"https://x.com"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestLinkHandler_Shorten_InvalidBody(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{})

	for _, body := range []string{`{"url":""}`, `not-json`} {
		req := withUserID(
			httptest.NewRequest("POST", "/api/links", bytes.NewBufferString(body)),
			1,
		)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: want 400, got %d", body, w.Code)
		}
	}
}

func TestLinkHandler_Shorten_InvalidURL(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{
		shortenFn: func(_ context.Context, _ int64, _ string, _ *time.Time) (*service.Link, error) {
			return nil, service.ErrInvalidURL
		},
	})

	req := withUserID(
		httptest.NewRequest("POST", "/api/links", bytes.NewBufferString(`{"url":"ftp://bad.com"}`)),
		1,
	)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestLinkHandler_Shorten_ExpiresAt_BadFormat(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{})

	req := withUserID(
		httptest.NewRequest("POST", "/api/links",
			bytes.NewBufferString(`{"url":"https://x.com","expires_at":"not-a-date"}`)),
		1,
	)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// --- Redirect tests ---

func TestLinkHandler_Redirect_OK(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{
		resolveFn: func(_ context.Context, _ string) (string, error) {
			return "https://example.com", nil
		},
	})

	req := httptest.NewRequest("GET", "/abc123", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Errorf("want 302, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://example.com" {
		t.Errorf("want Location 'https://example.com', got %q", loc)
	}
}

func TestLinkHandler_Redirect_NotFound(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{
		resolveFn: func(_ context.Context, _ string) (string, error) {
			return "", service.ErrNotFound
		},
	})

	req := httptest.NewRequest("GET", "/nope", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

// --- Delete tests ---

func TestLinkHandler_Delete_OK(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{
		deleteFn: func(_ context.Context, _ string, _ int64) error { return nil },
	})

	req := withUserID(httptest.NewRequest("DELETE", "/api/links/abc123", nil), 1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("want 204, got %d", w.Code)
	}
}

func TestLinkHandler_Delete_NotFound(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{
		deleteFn: func(_ context.Context, _ string, _ int64) error { return service.ErrNotFound },
	})

	req := withUserID(httptest.NewRequest("DELETE", "/api/links/missing", nil), 1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestLinkHandler_Delete_NoAuth(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{})

	req := httptest.NewRequest("DELETE", "/api/links/abc123", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

// --- List tests ---

func TestLinkHandler_List_OK(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{
		listByUserFn: func(_ context.Context, _ int64, _ int64, _ int) ([]*service.Link, error) {
			return []*service.Link{
				{ID: 1, ShortCode: "abc", OriginalURL: "https://x.com"},
			}, nil
		},
	})

	req := withUserID(httptest.NewRequest("GET", "/api/links", nil), 1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	links, ok := resp["links"].([]any)
	if !ok || len(links) != 1 {
		t.Errorf("want 1 link in response, got %v", resp["links"])
	}
}

func TestLinkHandler_List_InvalidCursor(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{})

	req := withUserID(httptest.NewRequest("GET", "/api/links?cursor=notanumber", nil), 1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestLinkHandler_List_NoAuth(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{})

	req := httptest.NewRequest("GET", "/api/links", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestLinkHandler_List_NextCursorWhenFull(t *testing.T) {
	links := make([]*service.Link, 20)
	for i := range links {
		links[i] = &service.Link{ID: int64(i + 1), ShortCode: "c", OriginalURL: "https://x.com"}
	}
	mux := newLinkMux(&mockLinkSvc{
		listByUserFn: func(_ context.Context, _ int64, _ int64, _ int) ([]*service.Link, error) {
			return links, nil
		},
	})

	req := withUserID(httptest.NewRequest("GET", "/api/links", nil), 1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	cursor, _ := resp["next_cursor"].(string)
	if cursor == "" {
		t.Error("want non-empty next_cursor when page is full")
	}
}

func TestLinkHandler_Shorten_ExpiryInPast(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{
		shortenFn: func(_ context.Context, _ int64, _ string, _ *time.Time) (*service.Link, error) {
			return nil, service.ErrInvalidExpiry
		},
	})

	req := withUserID(
		httptest.NewRequest("POST", "/api/links",
			bytes.NewBufferString(`{"url":"https://x.com","expires_at":"2000-01-01T00:00:00Z"}`)),
		1,
	)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestLinkHandler_Shorten_BodyTooLarge(t *testing.T) {
	mux := newLinkMux(&mockLinkSvc{})
	big := `{"url":"https://x.com/` + strings.Repeat("a", 2<<20) + `"}`

	req := withUserID(httptest.NewRequest("POST", "/api/links", strings.NewReader(big)), 1)
	w := httptest.NewRecorder()
	middleware.MaxBodySize(mux).ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("want 413, got %d", w.Code)
	}
}
