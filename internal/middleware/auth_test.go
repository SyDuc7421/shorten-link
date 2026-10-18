package middleware_test

import (
	"context"
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

type adminCheckerFunc func(userID int64) (bool, error)

func (f adminCheckerFunc) IsActiveAdmin(_ context.Context, userID int64) (bool, error) {
	return f(userID)
}

func TestRequireAdmin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		userID     int64 // 0 = no user in context
		isAdmin    bool
		checkErr   error
		wantStatus int
		wantNext   bool
	}{
		{name: "no user", wantStatus: http.StatusUnauthorized},
		{name: "not admin", userID: 2, wantStatus: http.StatusForbidden},
		{name: "check fails", userID: 2, checkErr: errors.New("db down"), wantStatus: http.StatusInternalServerError},
		{name: "admin", userID: 1, isAdmin: true, wantStatus: http.StatusOK, wantNext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotID int64
			checker := adminCheckerFunc(func(id int64) (bool, error) { gotID = id; return tc.isAdmin, tc.checkErr })
			called := false
			next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true })

			req := httptest.NewRequest("GET", "/api/admin/me", nil)
			if tc.userID != 0 {
				req = req.WithContext(ctxkey.SetUserID(req.Context(), tc.userID))
			}
			w := httptest.NewRecorder()
			middleware.RequireAdmin(checker)(next).ServeHTTP(w, req)

			if w.Code != tc.wantStatus || called != tc.wantNext {
				t.Fatalf("want %d next=%v, got %d next=%v", tc.wantStatus, tc.wantNext, w.Code, called)
			}
			if tc.userID != 0 && gotID != tc.userID {
				t.Errorf("checker got user %d, want %d", gotID, tc.userID)
			}
			if !tc.wantNext && w.Header().Get("Content-Type") != "application/json" {
				t.Errorf("want JSON content type, got %q", w.Header().Get("Content-Type"))
			}
			if tc.wantStatus == http.StatusForbidden && w.Body.String() != `{"error":"forbidden"}`+"\n" {
				t.Errorf("unexpected body %q", w.Body.String())
			}
		})
	}
}
