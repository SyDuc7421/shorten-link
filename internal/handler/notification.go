package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/service"
)

// NotificationServicer is the subset of NotificationService used by handlers.
type NotificationServicer interface {
	List(ctx context.Context, userID, beforeID int64) (*service.NotificationPage, error)
	MarkRead(ctx context.Context, userID, id int64) error
	MarkAllRead(ctx context.Context, userID int64) error
}

// NotificationHandler holds the HTTP handlers under /api/notifications.
type NotificationHandler struct {
	svc NotificationServicer
}

func NewNotificationHandler(svc NotificationServicer) *NotificationHandler {
	return &NotificationHandler{svc: svc}
}

// Register mounts the notification routes behind requireAuth.
func (h *NotificationHandler) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/notifications", requireAuth(http.HandlerFunc(h.List)))
	mux.Handle("POST /api/notifications/read-all", requireAuth(http.HandlerFunc(h.ReadAll)))
	mux.Handle("POST /api/notifications/{id}/read", requireAuth(http.HandlerFunc(h.MarkRead)))
}

func notificationJSON(n *service.Notification) map[string]any {
	var linkID any // null for password_changed
	if n.LinkID != nil {
		linkID = idString(*n.LinkID)
	}
	var code any
	if n.ShortCode != nil {
		code = *n.ShortCode
	}
	return map[string]any{
		"id":         idString(n.ID),
		"type":       n.Type,
		"link_id":    linkID,
		"code":       code,
		"expires_at": n.ExpiresAt,
		"read_at":    n.ReadAt,
		"created_at": n.CreatedAt,
	}
}

// List handles GET /api/notifications?cursor=
func (h *NotificationHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxkey.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var beforeID int64
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		beforeID = id
	}
	page, err := h.svc.List(r.Context(), userID, beforeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list notifications")
		return
	}
	out := make([]map[string]any, len(page.Items))
	for i, n := range page.Items {
		out[i] = notificationJSON(n)
	}
	var next string
	if page.HasMore && len(page.Items) > 0 {
		next = idString(page.Items[len(page.Items)-1].ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next, "unread_count": page.Unread})
}

// MarkRead handles POST /api/notifications/{id}/read
func (h *NotificationHandler) MarkRead(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxkey.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "notification not found")
		return
	}
	if err := h.svc.MarkRead(r.Context(), userID, id); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			writeError(w, http.StatusNotFound, "notification not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to mark notification read")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReadAll handles POST /api/notifications/read-all
func (h *NotificationHandler) ReadAll(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxkey.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.svc.MarkAllRead(r.Context(), userID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mark notifications read")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
