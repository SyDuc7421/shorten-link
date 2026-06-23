package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/middleware"
)

type mockTokenParser struct {
	userID int64
	err    error
}

func (m *mockTokenParser) ParseAccessToken(_ string) (int64, error) {
	return m.userID, m.err
}

func TestJWTAuth_ValidToken_SetsUserID(t *testing.T) {
	var gotUserID int64
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotUserID, _ = ctxkey.UserID(r.Context())
	})

	h := middleware.JWTAuth(&mockTokenParser{userID: 42})(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotUserID != 42 {
		t.Errorf("want userID 42, got %d", gotUserID)
	}
}

func TestJWTAuth_InvalidToken_DoesNotSetUserID(t *testing.T) {
	var gotUserID int64
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotUserID, _ = ctxkey.UserID(r.Context())
	})

	h := middleware.JWTAuth(&mockTokenParser{err: errors.New("bad token")})(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotUserID != 0 {
		t.Errorf("want userID 0 (not set), got %d", gotUserID)
	}
}

func TestJWTAuth_NoHeader_PassesThrough(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		reached = true
	})

	h := middleware.JWTAuth(&mockTokenParser{err: errors.New("should not be called")})(next)

	req := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !reached {
		t.Error("next handler should be reached even without Authorization header")
	}
}

func TestRequireAuth_WithUserID_Passes(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		reached = true
	})

	h := middleware.RequireAuth(next)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(ctxkey.SetUserID(req.Context(), 1))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !reached {
		t.Error("next handler should be reached when userID is in context")
	}
	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
}

func TestRequireAuth_WithoutUserID_Returns401(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next handler must NOT be called when userID is absent")
	})

	h := middleware.RequireAuth(next)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}
