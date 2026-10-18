package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/april/shorten-link/internal/handler"
	"github.com/april/shorten-link/internal/service"
)

// mockAdminSvc implements handler.AdminServicer. Unset functions panic, so a
// test only wires what it expects to be called.
type mockAdminSvc struct {
	meFn          func(ctx context.Context, userID int64) (*service.User, error)
	listUsersFn   func(ctx context.Context, f service.UserFilter) ([]*service.AdminUser, error)
	getUserFn     func(ctx context.Context, id int64) (*service.AdminUser, error)
	setQuotaFn    func(ctx context.Context, actorID, userID int64, quota int, reason string) (*service.AdminUser, error)
	disableUserFn func(ctx context.Context, actorID, userID int64, reason string) (*service.AdminUser, error)
	enableUserFn  func(ctx context.Context, actorID, userID int64, reason string) (*service.AdminUser, error)
	listLinksFn   func(ctx context.Context, f service.LinkFilter) ([]*service.AdminLink, error)
	disableLinkFn func(ctx context.Context, actorID int64, code, reason string) (*service.AdminLink, error)
	enableLinkFn  func(ctx context.Context, actorID int64, code, reason string) (*service.AdminLink, error)
	deleteLinkFn  func(ctx context.Context, actorID int64, code, reason string) error
	listAuditFn   func(ctx context.Context, f service.AuditFilter) ([]*service.AuditEntry, error)
}

func (m *mockAdminSvc) Me(ctx context.Context, userID int64) (*service.User, error) {
	return m.meFn(ctx, userID)
}
func (m *mockAdminSvc) ListUsers(ctx context.Context, f service.UserFilter) ([]*service.AdminUser, error) {
	return m.listUsersFn(ctx, f)
}
func (m *mockAdminSvc) GetUser(ctx context.Context, id int64) (*service.AdminUser, error) {
	return m.getUserFn(ctx, id)
}
func (m *mockAdminSvc) SetQuota(ctx context.Context, actorID, userID int64, quota int, reason string) (*service.AdminUser, error) {
	return m.setQuotaFn(ctx, actorID, userID, quota, reason)
}
func (m *mockAdminSvc) DisableUser(ctx context.Context, actorID, userID int64, reason string) (*service.AdminUser, error) {
	return m.disableUserFn(ctx, actorID, userID, reason)
}
func (m *mockAdminSvc) EnableUser(ctx context.Context, actorID, userID int64, reason string) (*service.AdminUser, error) {
	return m.enableUserFn(ctx, actorID, userID, reason)
}
func (m *mockAdminSvc) ListLinks(ctx context.Context, f service.LinkFilter) ([]*service.AdminLink, error) {
	return m.listLinksFn(ctx, f)
}
func (m *mockAdminSvc) DisableLink(ctx context.Context, actorID int64, code, reason string) (*service.AdminLink, error) {
	return m.disableLinkFn(ctx, actorID, code, reason)
}
func (m *mockAdminSvc) EnableLink(ctx context.Context, actorID int64, code, reason string) (*service.AdminLink, error) {
	return m.enableLinkFn(ctx, actorID, code, reason)
}
func (m *mockAdminSvc) DeleteLink(ctx context.Context, actorID int64, code, reason string) error {
	return m.deleteLinkFn(ctx, actorID, code, reason)
}
func (m *mockAdminSvc) ListAudit(ctx context.Context, f service.AuditFilter) ([]*service.AuditEntry, error) {
	return m.listAuditFn(ctx, f)
}

func pass(next http.Handler) http.Handler { return next }

func newAdminMux(svc handler.AdminServicer) *http.ServeMux {
	mux := http.NewServeMux()
	handler.NewAdminHandler(svc, "https://short.ly").Register(mux, pass, pass)
	return mux
}

// adminDo sends a request as admin user 1.
func adminDo(mux http.Handler, method, target, body string) *httptest.ResponseRecorder {
	req := withUserID(httptest.NewRequest(method, target, bytes.NewBufferString(body)), 1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// bigID is above 2^53, where a JSON number would lose precision in JavaScript.
const bigID int64 = 9007199254740993

func TestAdminHandler_Me(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{meFn: func(_ context.Context, id int64) (*service.User, error) {
		return &service.User{ID: bigID, Email: "a@x.com", Role: service.RoleAdmin}, nil
	}})
	w := adminDo(mux, "GET", "/api/admin/me", "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"email":"a@x.com","id":"9007199254740993","role":"admin"}` {
		t.Errorf("got %d %s", w.Code, w.Body.String())
	}
}

func TestAdminHandler_ListUsers_StringIDsAndCursor(t *testing.T) {
	var got service.UserFilter
	mux := newAdminMux(&mockAdminSvc{listUsersFn: func(_ context.Context, f service.UserFilter) ([]*service.AdminUser, error) {
		got = f
		users := make([]*service.AdminUser, service.AdminPageSize)
		for i := range users {
			users[i] = &service.AdminUser{ID: bigID - int64(i), Email: "u@x.com", Role: "user"}
		}
		return users, nil
	}})
	w := adminDo(mux, "GET", "/api/admin/users?q=x.com&status=disabled&cursor=123", "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", w.Code, w.Body.String())
	}
	if got.Query != "x.com" || got.Status != "disabled" || got.BeforeID != 123 {
		t.Errorf("filter not passed: %+v", got)
	}
	var resp struct {
		Users []map[string]any `json:"users"`
		Next  string           `json:"next_cursor"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp.Users[0]["id"] != "9007199254740993" {
		t.Errorf("id must be a string, got %#v", resp.Users[0]["id"])
	}
	if resp.Next != fmt.Sprint(bigID-int64(service.AdminPageSize-1)) {
		t.Errorf("next_cursor: got %q", resp.Next)
	}
	if _, ok := resp.Users[0]["links_today"]; ok {
		t.Error("list rows must not carry detail fields")
	}
}

func TestAdminHandler_LastPageHasEmptyCursor(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{listLinksFn: func(_ context.Context, _ service.LinkFilter) ([]*service.AdminLink, error) {
		return []*service.AdminLink{{ID: 5, ShortCode: "abc", OwnerID: 2, OwnerEmail: "o@x.com"}}, nil
	}})
	w := adminDo(mux, "GET", "/api/admin/links", "")
	var resp struct {
		Links []map[string]any `json:"links"`
		Next  string           `json:"next_cursor"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp.Next != "" {
		t.Errorf("want empty next_cursor, got %q", resp.Next)
	}
	link := resp.Links[0]
	owner, _ := link["owner"].(map[string]any)
	if link["short_url"] != "https://short.ly/abc" || owner["id"] != "2" || owner["email"] != "o@x.com" {
		t.Errorf("link JSON: %v", link)
	}
}

func TestAdminHandler_BadParams(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{})
	for _, target := range []string{
		"/api/admin/users/xyz",
		"/api/admin/users?cursor=abc",
		"/api/admin/users?status=weird",
		"/api/admin/links?user_id=x",
		"/api/admin/links?status=weird",
		"/api/admin/audit?actor_id=x",
		"/api/admin/audit?resource_type=weird",
	} {
		if w := adminDo(mux, "GET", target, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", target, w.Code)
		}
	}
}

func TestAdminHandler_UserNotFound(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{getUserFn: func(context.Context, int64) (*service.AdminUser, error) {
		return nil, fmt.Errorf("service: get user: %w", service.ErrNotFound)
	}})
	if w := adminDo(mux, "GET", "/api/admin/users/42", ""); w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestAdminHandler_GetUserDetail(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{getUserFn: func(_ context.Context, id int64) (*service.AdminUser, error) {
		return &service.AdminUser{ID: id, LinksToday: 3, TotalClicks: 9}, nil
	}})
	w := adminDo(mux, "GET", "/api/admin/users/42", "")
	var u map[string]any
	json.Unmarshal(w.Body.Bytes(), &u) //nolint:errcheck
	if w.Code != http.StatusOK || u["id"] != "42" || u["links_today"] != float64(3) || u["total_clicks"] != float64(9) {
		t.Errorf("got %d %v", w.Code, u)
	}
}

func TestAdminHandler_DisableSelf(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{disableUserFn: func(context.Context, int64, int64, string) (*service.AdminUser, error) {
		return nil, service.ErrSelfDisable
	}})
	if w := adminDo(mux, "POST", "/api/admin/users/1/disable", ""); w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

func TestAdminHandler_DisableUser_PassesActorAndReason(t *testing.T) {
	var actor, target int64
	var reason string
	mux := newAdminMux(&mockAdminSvc{disableUserFn: func(_ context.Context, a, u int64, r string) (*service.AdminUser, error) {
		actor, target, reason = a, u, r
		return &service.AdminUser{ID: u}, nil
	}})
	w := adminDo(mux, "POST", "/api/admin/users/42/disable", `{"reason":"spam"}`)
	if w.Code != http.StatusOK || actor != 1 || target != 42 || reason != "spam" {
		t.Errorf("got %d actor=%d target=%d reason=%q", w.Code, actor, target, reason)
	}
}

func TestAdminHandler_ReasonTooLong(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{enableUserFn: func(context.Context, int64, int64, string) (*service.AdminUser, error) {
		return nil, service.ErrReasonTooLong
	}})
	if w := adminDo(mux, "POST", "/api/admin/users/42/enable", `{"reason":"x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestAdminHandler_PatchQuota(t *testing.T) {
	var gotQuota int
	mux := newAdminMux(&mockAdminSvc{setQuotaFn: func(_ context.Context, _, id int64, q int, _ string) (*service.AdminUser, error) {
		if q > service.MaxQuotaDaily {
			return nil, service.ErrInvalidQuota
		}
		gotQuota = q
		return &service.AdminUser{ID: id, QuotaDaily: q}, nil
	}})
	if w := adminDo(mux, "PATCH", "/api/admin/users/42", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing quota: want 400, got %d", w.Code)
	}
	if w := adminDo(mux, "PATCH", "/api/admin/users/42", `{"quota_daily":100001}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid quota: want 400, got %d", w.Code)
	}
	w := adminDo(mux, "PATCH", "/api/admin/users/42", `{"quota_daily":0}`)
	if w.Code != http.StatusOK || gotQuota != 0 || !strings.Contains(w.Body.String(), `"quota_daily":0`) {
		t.Errorf("quota 0: got %d %s", w.Code, w.Body.String())
	}
}

func TestAdminHandler_DisableLink_EmptyBody(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{disableLinkFn: func(_ context.Context, _ int64, code, reason string) (*service.AdminLink, error) {
		return &service.AdminLink{ShortCode: code}, nil
	}})
	if w := adminDo(mux, "POST", "/api/admin/links/abc/disable", ""); w.Code != http.StatusOK {
		t.Errorf("want 200, got %d %s", w.Code, w.Body.String())
	}
	if w := adminDo(mux, "POST", "/api/admin/links/abc/disable", "{bad"); w.Code != http.StatusBadRequest {
		t.Errorf("malformed body: want 400, got %d", w.Code)
	}
}

func TestAdminHandler_DeleteLink(t *testing.T) {
	mux := newAdminMux(&mockAdminSvc{deleteLinkFn: func(_ context.Context, _ int64, code, _ string) error {
		if code == "nope" {
			return service.ErrNotFound
		}
		return nil
	}})
	if w := adminDo(mux, "DELETE", "/api/admin/links/abc", ""); w.Code != http.StatusNoContent {
		t.Errorf("want 204, got %d", w.Code)
	}
	if w := adminDo(mux, "DELETE", "/api/admin/links/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestAdminHandler_ListLinks_Filter(t *testing.T) {
	var got service.LinkFilter
	mux := newAdminMux(&mockAdminSvc{listLinksFn: func(_ context.Context, f service.LinkFilter) ([]*service.AdminLink, error) {
		got = f
		return nil, nil
	}})
	w := adminDo(mux, "GET", "/api/admin/links?q=evil&status=expired&user_id=7&cursor=9", "")
	if w.Code != http.StatusOK || got.Query != "evil" || got.Status != "expired" || got.UserID != 7 || got.BeforeID != 9 {
		t.Errorf("got %d %+v", w.Code, got)
	}
	if !strings.Contains(w.Body.String(), `"links":[]`) {
		t.Errorf("empty list must be [], got %s", w.Body.String())
	}
}

func TestAdminHandler_Audit(t *testing.T) {
	var got service.AuditFilter
	mux := newAdminMux(&mockAdminSvc{listAuditFn: func(_ context.Context, f service.AuditFilter) ([]*service.AuditEntry, error) {
		got = f
		return []*service.AuditEntry{
			{ID: 2, ActorID: 1, ActorEmail: "a@x.com", Action: "link.disable", ResourceType: "link", ResourceID: 10, Metadata: []byte(`{"code":"abc"}`)},
			{ID: 1, ActorID: 0, Action: "admin.grant", ResourceType: "user", ResourceID: 1, Metadata: []byte(`{}`)},
		}, nil
	}})
	w := adminDo(mux, "GET", "/api/admin/audit?actor_id=1&resource_type=link", "")
	if w.Code != http.StatusOK || got.ActorID != 1 || got.ResourceType != "link" {
		t.Fatalf("got %d %+v", w.Code, got)
	}
	var resp struct {
		Entries []map[string]any `json:"entries"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	actor, _ := resp.Entries[0]["actor"].(map[string]any)
	meta, _ := resp.Entries[0]["metadata"].(map[string]any)
	if actor["id"] != "1" || actor["email"] != "a@x.com" || meta["code"] != "abc" || resp.Entries[0]["resource_id"] != "10" {
		t.Errorf("entry 0: %v", resp.Entries[0])
	}
	if v, ok := resp.Entries[1]["actor"]; !ok || v != nil {
		t.Errorf("system actor must be null, got %#v", v)
	}
}

func TestAdminHandler_RoutesRequireAdmin(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	}
	mux := http.NewServeMux()
	handler.NewAdminHandler(&mockAdminSvc{}, "https://short.ly").Register(mux, pass, deny)
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/admin/me"},
		{"GET", "/api/admin/users"},
		{"GET", "/api/admin/users/1"},
		{"PATCH", "/api/admin/users/1"},
		{"POST", "/api/admin/users/1/disable"},
		{"POST", "/api/admin/users/1/enable"},
		{"GET", "/api/admin/links"},
		{"POST", "/api/admin/links/abc/disable"},
		{"POST", "/api/admin/links/abc/enable"},
		{"DELETE", "/api/admin/links/abc"},
		{"GET", "/api/admin/audit"},
	} {
		if w := adminDo(mux, r.method, r.path, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s %s: want 403, got %d", r.method, r.path, w.Code)
		}
	}
}
