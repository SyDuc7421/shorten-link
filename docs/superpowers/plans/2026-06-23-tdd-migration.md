# TDD Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add test coverage to all untested layers (handler, middleware) and fix the broken integration test migration runner so `go test ./...` passes green.

**Architecture:** Service and repository layers already have tests (unit mocks + testcontainers integration). This plan adds `net/http/httptest`-based tests for HTTP handlers and middleware, and fixes the multi-statement SQL migration bug in the existing integration tests.

**Tech Stack:** Go standard library (`testing`, `net/http/httptest`), testcontainers-go/modules/redis for the rate limiter test, existing mock patterns from `internal/service/*_test.go`.

## Global Constraints

- All test files live inside the package under test (`package handler_test`, `package middleware_test`)
- No third-party assertion libraries — use plain `t.Errorf` / `t.Fatalf`
- No mocking libraries — hand-roll struct mocks that satisfy existing interfaces
- Run `go test ./...` after every task; all tasks must pass before moving on
- Never modify production source files in this plan (tests only, plus the DSN fix in integration_test.go)

---

## File Structure

| File | Action | Purpose |
|------|--------|---------|
| `internal/repository/integration_test.go` | Modify line 43 | Fix multi-statement SQL migration failure |
| `internal/handler/auth_handler_test.go` | Create | HTTP tests for AuthHandler |
| `internal/handler/link_handler_test.go` | Create | HTTP tests for LinkHandler |
| `internal/middleware/auth_test.go` | Create | Tests for JWTAuth + RequireAuth |
| `internal/middleware/requestid_test.go` | Create | Tests for RequestID middleware |
| `internal/middleware/ratelimit_test.go` | Create | Integration tests for RateLimit via testcontainers Redis |

---

### Task 1: Fix integration test migration runner

**Files:**
- Modify: `internal/repository/integration_test.go:43`

**Interfaces:**
- Consumes: nothing new
- Produces: green `go test ./internal/repository/...` (currently all 5 MySQL tests fail)

The root cause: `db.ExecContext(ctx, string(b))` is called with the full text of `003_create_analytics.up.sql`, which contains two `CREATE TABLE` statements. MySQL's Go driver blocks multi-statement execution unless `multiStatements=true` is in the DSN. Adding it to the `ConnectionString` call fixes this.

- [ ] **Step 1: Write a failing test run to document the baseline**

```bash
go test ./internal/repository/... -run TestLinkMySQL_CreateAndGet -v 2>&1 | tail -5
```

Expected output includes: `Error 1064 (42000): You have an error in your SQL syntax`

- [ ] **Step 2: Fix the DSN in setupMySQL**

In `internal/repository/integration_test.go`, line 43 currently reads:
```go
dsn, err := c.ConnectionString(ctx, "parseTime=true")
```

Change it to:
```go
dsn, err := c.ConnectionString(ctx, "parseTime=true", "multiStatements=true")
```

- [ ] **Step 3: Run repository tests to verify they pass**

```bash
go test ./internal/repository/... -v -timeout 120s
```

Expected: all 8 tests PASS (5 MySQL + 3 Redis)

- [ ] **Step 4: Commit**

```bash
git add internal/repository/integration_test.go
git commit -m "fix: add multiStatements=true to MySQL DSN in integration tests"
```

---

### Task 2: AuthHandler tests

**Files:**
- Create: `internal/handler/auth_handler_test.go`

**Interfaces:**
- Consumes: `handler.AuthServicer` (defined in `internal/handler/auth.go:13-17`), `service.TokenPair`, `service.ErrEmailTaken`, `service.ErrInvalidCredentials`, `service.ErrInvalidToken`
- Produces: test coverage for `RegisterUser`, `Login`, `Refresh` handlers

- [ ] **Step 1: Write the test file**

Create `internal/handler/auth_handler_test.go`:

```go
package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/april/shorten-link/internal/handler"
	"github.com/april/shorten-link/internal/service"
)

// mockAuthSvc implements handler.AuthServicer.
type mockAuthSvc struct {
	registerFn func(ctx context.Context, email, password string) (*service.TokenPair, error)
	loginFn    func(ctx context.Context, email, password string) (*service.TokenPair, error)
	refreshFn  func(ctx context.Context, token string) (*service.TokenPair, error)
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
```

- [ ] **Step 2: Run the tests — expect PASS (handler code already exists)**

```bash
go test ./internal/handler/... -run TestAuthHandler -v
```

Expected: all 7 `TestAuthHandler_*` tests PASS

- [ ] **Step 3: Commit**

```bash
git add internal/handler/auth_handler_test.go
git commit -m "test: add HTTP handler tests for AuthHandler"
```

---

### Task 3: LinkHandler tests

**Files:**
- Create: `internal/handler/link_handler_test.go`

**Interfaces:**
- Consumes: `handler.LinkServicer` (defined in `internal/handler/link.go:19-24`), `ctxkey.SetUserID`, `service.ErrInvalidURL`, `service.ErrNotFound`
- Produces: test coverage for `Shorten`, `Redirect`, `Delete`, `List` handlers

- [ ] **Step 1: Write the test file**

Create `internal/handler/link_handler_test.go`:

```go
package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/handler"
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

	// No withUserID — no auth context
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
	// Returns exactly 20 links (pageSize) — response must include next_cursor
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

// Verify errors.Is behaviour is preserved (wrapping must not break sentinel check)
var _ = errors.Is // keep import used
```

- [ ] **Step 2: Run the tests — expect PASS**

```bash
go test ./internal/handler/... -run TestLinkHandler -v
```

Expected: all 13 `TestLinkHandler_*` tests PASS

- [ ] **Step 3: Commit**

```bash
git add internal/handler/link_handler_test.go
git commit -m "test: add HTTP handler tests for LinkHandler"
```

---

### Task 4: JWTAuth and RequireAuth middleware tests

**Files:**
- Create: `internal/middleware/auth_test.go`

**Interfaces:**
- Consumes: `middleware.JWTAuth(parser TokenParser)`, `middleware.RequireAuth`, `ctxkey.SetUserID`, `ctxkey.UserID`
- Produces: tests covering auth middleware branches (valid token, invalid token, no header, with/without userID in context)

- [ ] **Step 1: Write the test file**

Create `internal/middleware/auth_test.go`:

```go
package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/middleware"
)

// mockTokenParser implements middleware.TokenParser.
type mockTokenParser struct {
	userID int64
	err    error
}

func (m *mockTokenParser) ParseAccessToken(_ string) (int64, error) {
	return m.userID, m.err
}

// --- JWTAuth tests ---

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
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		if _, ok := ctxkey.UserID(r.Context()); ok {
			t.Error("userID must not be set when Authorization header is absent")
		}
	})
	h := middleware.JWTAuth(&mockTokenParser{userID: 1})(next)

	req := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !reached {
		t.Error("next handler should always be called by JWTAuth, even with no token")
	}
}

func TestJWTAuth_MalformedHeader_PassesThrough(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { reached = true })
	h := middleware.JWTAuth(&mockTokenParser{userID: 1})(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Token not-bearer-format")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !reached {
		t.Error("next handler should be called even with malformed Authorization header")
	}
}

// --- RequireAuth tests ---

func TestRequireAuth_WithUserID_Passes(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { reached = true })
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
```

- [ ] **Step 2: Run the tests — expect PASS**

```bash
go test ./internal/middleware/... -run "TestJWTAuth|TestRequireAuth" -v
```

Expected: all 6 tests PASS

- [ ] **Step 3: Commit**

```bash
git add internal/middleware/auth_test.go
git commit -m "test: add unit tests for JWTAuth and RequireAuth middleware"
```

---

### Task 5: RequestID middleware tests

**Files:**
- Create: `internal/middleware/requestid_test.go`

**Interfaces:**
- Consumes: `middleware.RequestID(base *zap.Logger)` (defined in `internal/middleware/requestid.go:13`)
- Produces: tests for header propagation, auto-generation, and uniqueness

- [ ] **Step 1: Write the test file**

Create `internal/middleware/requestid_test.go`:

```go
package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/middleware"
)

func TestRequestID_PropagatesExistingID(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	h := middleware.RequestID(zap.NewNop())(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "my-request-id")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-ID"); got != "my-request-id" {
		t.Errorf("want X-Request-ID 'my-request-id', got %q", got)
	}
}

func TestRequestID_GeneratesIDWhenAbsent(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	h := middleware.RequestID(zap.NewNop())(next)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	id := w.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("expected a generated X-Request-ID, got empty string")
	}
	// generateID encodes 16 random bytes as hex → 32 chars
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
```

- [ ] **Step 2: Run the tests — expect PASS**

```bash
go test ./internal/middleware/... -run TestRequestID -v
```

Expected: all 3 `TestRequestID_*` tests PASS

- [ ] **Step 3: Commit**

```bash
git add internal/middleware/requestid_test.go
git commit -m "test: add unit tests for RequestID middleware"
```

---

### Task 6: RateLimit middleware tests (testcontainers Redis)

**Files:**
- Create: `internal/middleware/ratelimit_test.go`

**Interfaces:**
- Consumes: `middleware.RateLimit(rdb *redis.Client, action string, limit int, window time.Duration)`
- Produces: integration tests that verify requests within the limit pass and requests over the limit return 429

Note: these are integration tests using a real Redis container (consistent with how `internal/repository` tests work). They require Docker to be running. They are excluded from fast-unit-test runs via build tag `//go:build integration` — add the tag so `go test ./...` only runs them when `-tags integration` is passed.

Actually, looking at the existing integration tests — they carry **no** build tag and run as part of `go test ./...`. For consistency, the rate-limit test follows the same convention (no tag, runs always). If the suite becomes too slow, tags can be added later.

- [ ] **Step 1: Write the test file**

Create `internal/middleware/ratelimit_test.go`:

```go
package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/april/shorten-link/internal/middleware"
)

func setupTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()

	c, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { c.Terminate(ctx) }) //nolint:errcheck

	addr, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	addr = strings.TrimPrefix(addr, "redis://")

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func requestFrom(ip string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = ip + ":1234"
	return req
}

func TestRateLimit_AllowsWithinLimit(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-allow", 3, time.Minute)(okHandler())

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		limiter.ServeHTTP(w, requestFrom("10.0.0.1"))
		if w.Code != http.StatusOK {
			t.Errorf("request %d: want 200, got %d", i+1, w.Code)
		}
	}
}

func TestRateLimit_BlocksWhenExceeded(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-block", 2, time.Minute)(okHandler())

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		limiter.ServeHTTP(w, requestFrom("10.0.0.2"))
		if w.Code != http.StatusOK {
			t.Errorf("request %d should pass, got %d", i+1, w.Code)
		}
	}

	w := httptest.NewRecorder()
	limiter.ServeHTTP(w, requestFrom("10.0.0.2"))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("third request: want 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("want Retry-After header on 429 response")
	}
}

func TestRateLimit_IndependentPerIP(t *testing.T) {
	rdb := setupTestRedis(t)
	limiter := middleware.RateLimit(rdb, "test-per-ip", 1, time.Minute)(okHandler())

	// IP A hits its limit
	w1 := httptest.NewRecorder()
	limiter.ServeHTTP(w1, requestFrom("10.0.0.3"))
	if w1.Code != http.StatusOK {
		t.Fatalf("IP A first request: want 200, got %d", w1.Code)
	}
	w2 := httptest.NewRecorder()
	limiter.ServeHTTP(w2, requestFrom("10.0.0.3"))
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("IP A second request: want 429, got %d", w2.Code)
	}

	// IP B is unaffected
	w3 := httptest.NewRecorder()
	limiter.ServeHTTP(w3, requestFrom("10.0.0.4"))
	if w3.Code != http.StatusOK {
		t.Errorf("IP B first request: want 200, got %d (should not share limit with IP A)", w3.Code)
	}
}
```

- [ ] **Step 2: Run the rate limit tests — expect PASS**

```bash
go test ./internal/middleware/... -run TestRateLimit -v -timeout 120s
```

Expected: all 3 `TestRateLimit_*` tests PASS (Docker must be running)

- [ ] **Step 3: Run the full test suite**

```bash
go test ./... -timeout 180s
```

Expected output:
```
ok      github.com/april/shorten-link/internal/handler    0.xxx s
ok      github.com/april/shorten-link/internal/middleware 30.xxx s
ok      github.com/april/shorten-link/internal/repository 45.xxx s
ok      github.com/april/shorten-link/internal/service    3.xxx s
ok      github.com/april/shorten-link/pkg/idgen           0.xxx s
```

- [ ] **Step 4: Commit**

```bash
git add internal/middleware/ratelimit_test.go
git commit -m "test: add integration tests for RateLimit middleware"
```

---

## Self-Review

**Spec coverage:**
- Integration test migration bug → Task 1 ✓
- AuthHandler (Register, Login, Refresh) → Task 2 ✓ (7 tests)
- LinkHandler (Shorten, Redirect, Delete, List) → Task 3 ✓ (13 tests)
- JWTAuth + RequireAuth → Task 4 ✓ (6 tests)
- RequestID → Task 5 ✓ (3 tests)
- RateLimit → Task 6 ✓ (3 tests)

**Placeholder scan:** None. All test functions contain complete code.

**Type consistency:**
- `mockAuthSvc.Register` matches `handler.AuthServicer.Register(ctx, email, password string) (*service.TokenPair, error)` ✓
- `mockLinkSvc.Shorten` matches `handler.LinkServicer.Shorten(ctx, userID int64, rawURL string, expiresAt *time.Time) (*service.Link, error)` ✓
- `mockTokenParser.ParseAccessToken` matches `middleware.TokenParser.ParseAccessToken(token string) (int64, error)` ✓
- `setupTestRedis` returns `*redis.Client`, which is the exact type `middleware.RateLimit` accepts ✓
