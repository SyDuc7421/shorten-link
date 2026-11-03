package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/april/shorten-link/internal/handler"
	"github.com/april/shorten-link/internal/service"
)

type mockNotifSvc struct {
	listFn    func(ctx context.Context, userID, beforeID int64) ([]*service.Notification, int64, error)
	markFn    func(ctx context.Context, userID, id int64) error
	markAllFn func(ctx context.Context, userID int64) error
}

func (m *mockNotifSvc) List(ctx context.Context, userID, beforeID int64) ([]*service.Notification, int64, error) {
	return m.listFn(ctx, userID, beforeID)
}
func (m *mockNotifSvc) MarkRead(ctx context.Context, userID, id int64) error {
	return m.markFn(ctx, userID, id)
}
func (m *mockNotifSvc) MarkAllRead(ctx context.Context, userID int64) error {
	return m.markAllFn(ctx, userID)
}

func newNotifMux(svc handler.NotificationServicer) *http.ServeMux {
	mux := http.NewServeMux()
	handler.NewNotificationHandler(svc).Register(mux, pass)
	return mux
}

// notifDo sends a request as user 42, or unauthenticated when anon is true.
func notifDo(mux http.Handler, method, target string, anon bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if !anon {
		req = withUserID(req, 42)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func notifRows(n int) []*service.Notification {
	rows := make([]*service.Notification, n)
	for i := range rows {
		rows[i] = &service.Notification{ID: bigID - int64(i), Type: service.NotificationPasswordChanged, CreatedAt: time.Now()}
	}
	return rows
}

func TestNotifications_List(t *testing.T) {
	linkID, code, exp := int64(734), "abc", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	var gotUser, gotBefore int64
	rows := notifRows(3)
	rows[0] = &service.Notification{ID: 12, Type: service.NotificationLinkExpiring, LinkID: &linkID, ShortCode: &code, ExpiresAt: &exp, CreatedAt: exp}
	mux := newNotifMux(&mockNotifSvc{listFn: func(_ context.Context, u, b int64) ([]*service.Notification, int64, error) {
		gotUser, gotBefore = u, b
		return rows, 3, nil
	}})
	w := notifDo(mux, "GET", "/api/notifications?cursor=99", false)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if gotUser != 42 || gotBefore != 99 {
		t.Errorf("user=%d before=%d", gotUser, gotBefore)
	}
	var resp struct {
		Items  []map[string]any `json:"items"`
		Next   string           `json:"next_cursor"`
		Unread int64            `json:"unread_count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp.Next != "" || resp.Unread != 3 || len(resp.Items) != 3 {
		t.Errorf("bad envelope: %s", w.Body.String())
	}
	if it := resp.Items[0]; it["id"] != "12" || it["type"] != "link_expiring" || it["link_id"] != "734" || it["code"] != "abc" || it["read_at"] != nil {
		t.Errorf("bad link item: %#v", it)
	}
	pw := resp.Items[1]
	if pw["id"] != "9007199254740992" {
		t.Errorf("id must be a string: %#v", pw["id"])
	}
	if v, ok := pw["link_id"]; !ok || v != nil {
		t.Errorf("link_id must be present and null: %#v", pw)
	}
	if v, ok := pw["code"]; !ok || v != nil {
		t.Errorf("code must be present and null: %#v", pw)
	}

	// a full page sets next_cursor to the last id
	full := notifRows(service.NotificationPageSize)
	mux = newNotifMux(&mockNotifSvc{listFn: func(context.Context, int64, int64) ([]*service.Notification, int64, error) {
		return full, 0, nil
	}})
	w = notifDo(mux, "GET", "/api/notifications", false)
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp.Next != idString(full[len(full)-1].ID) {
		t.Errorf("next_cursor = %q", resp.Next)
	}
}

func idString(id int64) string { b, _ := json.Marshal(id); return string(b) }

func TestNotifications_ListBadCursor(t *testing.T) {
	w := notifDo(newNotifMux(&mockNotifSvc{}), "GET", "/api/notifications?cursor=abc", false)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d", w.Code)
	}
}

func TestNotifications_MarkRead(t *testing.T) {
	var gotUser, gotID int64
	mux := newNotifMux(&mockNotifSvc{markFn: func(_ context.Context, u, id int64) error {
		gotUser, gotID = u, id
		return nil
	}})
	w := notifDo(mux, "POST", "/api/notifications/17/read", false)
	if w.Code != http.StatusNoContent || gotUser != 42 || gotID != 17 {
		t.Errorf("code=%d user=%d id=%d", w.Code, gotUser, gotID)
	}
}

func TestNotifications_MarkReadNotFound(t *testing.T) {
	mux := newNotifMux(&mockNotifSvc{markFn: func(context.Context, int64, int64) error { return service.ErrNotFound }})
	if w := notifDo(mux, "POST", "/api/notifications/17/read", false); w.Code != http.StatusNotFound {
		t.Errorf("missing: got %d", w.Code)
	}
	if w := notifDo(mux, "POST", "/api/notifications/abc/read", false); w.Code != http.StatusNotFound {
		t.Errorf("non-numeric: got %d", w.Code)
	}
}

func TestNotifications_ReadAll(t *testing.T) {
	var got int64
	mux := newNotifMux(&mockNotifSvc{markAllFn: func(_ context.Context, u int64) error { got = u; return nil }})
	w := notifDo(mux, "POST", "/api/notifications/read-all", false)
	if w.Code != http.StatusNoContent || got != 42 {
		t.Errorf("code=%d user=%d", w.Code, got)
	}
}

func TestNotifications_RequireAuth(t *testing.T) {
	mux := newNotifMux(&mockNotifSvc{})
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/notifications"},
		{"POST", "/api/notifications/1/read"},
		{"POST", "/api/notifications/read-all"},
	} {
		if w := notifDo(mux, rt.method, rt.path, true); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: got %d", rt.method, rt.path, w.Code)
		}
	}
}
