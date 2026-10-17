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

	"github.com/april/shorten-link/internal/handler"
	"github.com/april/shorten-link/internal/middleware"
	"github.com/april/shorten-link/internal/service"
)

// mockAuthSvc implements handler.AuthServicer.
type mockAuthSvc struct {
	registerFn func(ctx context.Context, email, password string) (*service.TokenPair, error)
	loginFn    func(ctx context.Context, email, password string) (*service.TokenPair, error)
	refreshFn  func(ctx context.Context, token string) (*service.TokenPair, error)
	forgotFn   func(ctx context.Context, email string)
	resetFn    func(ctx context.Context, token, password string) error
}

func (m *mockAuthSvc) RequestPasswordReset(ctx context.Context, email string) {
	m.forgotFn(ctx, email)
}
func (m *mockAuthSvc) ResetPassword(ctx context.Context, token, password string) error {
	return m.resetFn(ctx, token, password)
}

func (m *mockAuthSvc) Register(ctx context.Context, email, password string) (*service.TokenPair, error) {
	return m.registerFn(ctx, email, password)
}
func (m *mockAuthSvc) Login(ctx context.Context, email, password string) (*service.TokenPair, error) {
	return m.loginFn(ctx, email, password)
}
func (m *mockAuthSvc) Refresh(ctx context.Context, token string) (*service.TokenPair, error) {
	return m.refreshFn(ctx, token)
}

func okTokenPair() *service.TokenPair {
	return &service.TokenPair{AccessToken: "acc", RefreshToken: "ref"}
}

func newAuthMux(svc handler.AuthServicer) *http.ServeMux {
	mux := http.NewServeMux()
	handler.NewAuthHandler(svc).Register(mux)
	return mux
}

// --- Register tests ---

func TestAuthHandler_Register_OK(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{
		registerFn: func(_ context.Context, _, _ string) (*service.TokenPair, error) {
			return okTokenPair(), nil
		},
	})

	req := httptest.NewRequest("POST", "/api/auth/register",
		bytes.NewBufferString(`{"email":"a@b.com","password":"secret123"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("want 201, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp["access_token"] != "acc" {
		t.Errorf("want access_token 'acc', got %q", resp["access_token"])
	}
	if resp["refresh_token"] != "ref" {
		t.Errorf("want refresh_token 'ref', got %q", resp["refresh_token"])
	}
}

func TestAuthHandler_Register_EmptyFields(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{})

	for _, body := range []string{
		`{"email":"","password":"pass"}`,
		`{"email":"a@b.com","password":""}`,
		`{}`,
		`not-json`,
	} {
		req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: want 400, got %d", body, w.Code)
		}
	}
}

func TestAuthHandler_Register_DuplicateEmail(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{
		registerFn: func(_ context.Context, _, _ string) (*service.TokenPair, error) {
			return nil, service.ErrEmailTaken
		},
	})

	req := httptest.NewRequest("POST", "/api/auth/register",
		bytes.NewBufferString(`{"email":"a@b.com","password":"secret123"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

// --- Login tests ---

func TestAuthHandler_Login_OK(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{
		loginFn: func(_ context.Context, _, _ string) (*service.TokenPair, error) {
			return okTokenPair(), nil
		},
	})

	req := httptest.NewRequest("POST", "/api/auth/login",
		bytes.NewBufferString(`{"email":"a@b.com","password":"secret123"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", w.Code, w.Body)
	}
}

func TestAuthHandler_Login_InvalidCredentials(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{
		loginFn: func(_ context.Context, _, _ string) (*service.TokenPair, error) {
			return nil, service.ErrInvalidCredentials
		},
	})

	req := httptest.NewRequest("POST", "/api/auth/login",
		bytes.NewBufferString(`{"email":"a@b.com","password":"wrong"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestAuthHandler_Login_EmptyFields(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{})

	req := httptest.NewRequest("POST", "/api/auth/login",
		bytes.NewBufferString(`{"email":"","password":""}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// --- Refresh tests ---

func TestAuthHandler_Refresh_OK(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{
		refreshFn: func(_ context.Context, _ string) (*service.TokenPair, error) {
			return okTokenPair(), nil
		},
	})

	req := httptest.NewRequest("POST", "/api/auth/refresh",
		bytes.NewBufferString(`{"refresh_token":"some-refresh-token"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", w.Code, w.Body)
	}
}

func TestAuthHandler_Refresh_InvalidToken(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{
		refreshFn: func(_ context.Context, _ string) (*service.TokenPair, error) {
			return nil, service.ErrInvalidToken
		},
	})

	req := httptest.NewRequest("POST", "/api/auth/refresh",
		bytes.NewBufferString(`{"refresh_token":"expired"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestAuthHandler_Refresh_MissingToken(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{})

	req := httptest.NewRequest("POST", "/api/auth/refresh",
		bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestAuthHandler_Register_WeakPassword(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{
		registerFn: func(_ context.Context, _, _ string) (*service.TokenPair, error) {
			return nil, service.ErrWeakPassword
		},
	})

	req := httptest.NewRequest("POST", "/api/auth/register",
		bytes.NewBufferString(`{"email":"a@b.com","password":"short"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestAuthHandler_Register_BodyTooLarge(t *testing.T) {
	mux := newAuthMux(&mockAuthSvc{})
	big := `{"email":"a@b.com","password":"` + strings.Repeat("a", 2<<20) + `"}`

	req := httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(big))
	w := httptest.NewRecorder()
	middleware.MaxBodySize(mux).ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("want 413, got %d", w.Code)
	}
}

// --- password reset ---

func postJSON(mux http.Handler, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewBufferString(body)))
	return w
}

func TestAuthHandler_ForgotPassword_Accepted(t *testing.T) {
	var got string
	mux := newAuthMux(&mockAuthSvc{forgotFn: func(_ context.Context, email string) { got = email }})
	w := postJSON(mux, "/api/auth/forgot-password", `{"email":"a@x.com"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body)
	}
	if w.Body.Len() != 0 {
		t.Errorf("want empty body, got %q", w.Body)
	}
	if got != "a@x.com" {
		t.Errorf("service got email %q", got)
	}
}

func TestAuthHandler_ForgotPassword_Errors(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing email", `{}`},
		{"bad json", `{`},
	} {
		mux := newAuthMux(&mockAuthSvc{forgotFn: func(context.Context, string) { t.Error("service called for a bad request") }})
		if w := postJSON(mux, "/api/auth/forgot-password", tc.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", tc.name, w.Code)
		}
	}
}

func TestAuthHandler_ResetPassword(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		svcErr     error
		want       int
	}{
		{"ok", `{"token":"t","password":"new-password"}`, nil, http.StatusNoContent},
		{"missing token", `{"password":"new-password"}`, nil, http.StatusBadRequest},
		{"missing password", `{"token":"t"}`, nil, http.StatusBadRequest},
		{"weak password", `{"token":"t","password":"x"}`, service.ErrWeakPassword, http.StatusBadRequest},
		{"bad token", `{"token":"t","password":"new-password"}`, service.ErrInvalidToken, http.StatusUnauthorized},
		{"service failure", `{"token":"t","password":"new-password"}`, errors.New("db down"), http.StatusInternalServerError},
	} {
		mux := newAuthMux(&mockAuthSvc{resetFn: func(context.Context, string, string) error { return tc.svcErr }})
		if w := postJSON(mux, "/api/auth/reset-password", tc.body); w.Code != tc.want {
			t.Errorf("%s: want %d, got %d: %s", tc.name, tc.want, w.Code, w.Body)
		}
	}
}
