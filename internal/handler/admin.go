package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/service"
)

// AdminServicer is the subset of AdminService used by handlers.
// Defined here (consumer site) so handlers can be tested with a mock.
type AdminServicer interface {
	Me(ctx context.Context, userID int64) (*service.User, error)
	ListUsers(ctx context.Context, f service.UserFilter) ([]*service.AdminUser, error)
	GetUser(ctx context.Context, id int64) (*service.AdminUser, error)
	SetQuota(ctx context.Context, actorID, userID int64, quota int, reason string) (*service.AdminUser, error)
	DisableUser(ctx context.Context, actorID, userID int64, reason string) (*service.AdminUser, error)
	EnableUser(ctx context.Context, actorID, userID int64, reason string) (*service.AdminUser, error)
	ListLinks(ctx context.Context, f service.LinkFilter) ([]*service.AdminLink, error)
	DisableLink(ctx context.Context, actorID int64, code, reason string) (*service.AdminLink, error)
	EnableLink(ctx context.Context, actorID int64, code, reason string) (*service.AdminLink, error)
	DeleteLink(ctx context.Context, actorID int64, code, reason string) error
	ListAudit(ctx context.Context, f service.AuditFilter) ([]*service.AuditEntry, error)
}

// AdminHandler holds the HTTP handlers under /api/admin.
type AdminHandler struct {
	svc     AdminServicer
	baseURL string
}

// NewAdminHandler constructs an AdminHandler.
func NewAdminHandler(svc AdminServicer, baseURL string) *AdminHandler {
	return &AdminHandler{svc: svc, baseURL: strings.TrimRight(baseURL, "/")}
}

// Register mounts the admin routes. Every route runs requireAuth, then
// requireAdmin.
func (h *AdminHandler) Register(mux *http.ServeMux, requireAuth, requireAdmin func(http.Handler) http.Handler) {
	guard := func(fn http.HandlerFunc) http.Handler { return requireAuth(requireAdmin(fn)) }

	mux.Handle("GET /api/admin/me", guard(h.Me))
	mux.Handle("GET /api/admin/users", guard(h.ListUsers))
	mux.Handle("GET /api/admin/users/{id}", guard(h.GetUser))
	mux.Handle("PATCH /api/admin/users/{id}", guard(h.PatchUser))
	mux.Handle("POST /api/admin/users/{id}/disable", guard(h.DisableUser))
	mux.Handle("POST /api/admin/users/{id}/enable", guard(h.EnableUser))
	mux.Handle("GET /api/admin/links", guard(h.ListLinks))
	mux.Handle("POST /api/admin/links/{code}/disable", guard(h.DisableLink))
	mux.Handle("POST /api/admin/links/{code}/enable", guard(h.EnableLink))
	mux.Handle("DELETE /api/admin/links/{code}", guard(h.DeleteLink))
	mux.Handle("GET /api/admin/audit", guard(h.ListAudit))
}

// --- JSON shapes. IDs are strings: snowflake IDs exceed 2^53, which a
// JavaScript number cannot hold exactly. ---

func idString(id int64) string { return strconv.FormatInt(id, 10) }

func adminUserJSON(u *service.AdminUser, detail bool) map[string]any {
	m := map[string]any{
		"id":          idString(u.ID),
		"email":       u.Email,
		"role":        u.Role,
		"quota_daily": u.QuotaDaily,
		"disabled_at": u.DisabledAt,
		"created_at":  u.CreatedAt,
		"link_count":  u.LinkCount,
	}
	if detail {
		m["links_today"] = u.LinksToday
		m["total_clicks"] = u.TotalClicks
	}
	return m
}

func (h *AdminHandler) adminLinkJSON(l *service.AdminLink) map[string]any {
	return map[string]any{
		"id":           idString(l.ID),
		"code":         l.ShortCode,
		"short_url":    fmt.Sprintf("%s/%s", h.baseURL, l.ShortCode),
		"original_url": l.OriginalURL,
		"clicks":       l.Clicks,
		"owner": map[string]any{
			"id":          idString(l.OwnerID),
			"email":       l.OwnerEmail,
			"disabled_at": l.OwnerDisabledAt,
		},
		"disabled_at": l.DisabledAt,
		"expires_at":  l.ExpiresAt,
		"created_at":  l.CreatedAt,
	}
}

func auditJSON(e *service.AuditEntry) map[string]any {
	var actor any // null for system (CLI) entries
	if e.ActorID != 0 {
		actor = map[string]any{"id": idString(e.ActorID), "email": e.ActorEmail}
	}
	meta := e.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	return map[string]any{
		"id":            idString(e.ID),
		"actor":         actor,
		"action":        e.Action,
		"resource_type": e.ResourceType,
		"resource_id":   idString(e.ResourceID),
		"metadata":      meta,
		"created_at":    e.CreatedAt,
	}
}

// nextCursor is the last row's ID when the page is full, "" on the last page.
func nextCursor(n int, lastID func() int64) string {
	if n < service.AdminPageSize {
		return ""
	}
	return idString(lastID())
}

// --- input helpers ---

// errBadParam carries a 400 message.
type errBadParam string

func (e errBadParam) Error() string { return string(e) }

func parseIDParam(raw, name string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errBadParam("invalid " + name)
	}
	return id, nil
}

func parseEnum(raw, name string, allowed ...string) (string, error) {
	if raw == "" {
		return "", nil
	}
	for _, a := range allowed {
		if raw == a {
			return raw, nil
		}
	}
	return "", errBadParam(fmt.Sprintf("%s must be one of %s", name, strings.Join(allowed, ", ")))
}

// actor returns the signed-in admin's ID; RequireAuth guarantees it is set.
func actor(r *http.Request) int64 {
	id, _ := ctxkey.UserID(r.Context())
	return id
}

// decodeOptional decodes a JSON body into v. An empty body is allowed.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return true
	}
	if writeIfTooLarge(w, err) {
		return false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// writeAdminError maps service errors to status codes.
func writeAdminError(w http.ResponseWriter, err error, notFoundMsg string) {
	var bad errBadParam
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, string(bad))
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, notFoundMsg)
	case errors.Is(err, service.ErrSelfDisable):
		writeError(w, http.StatusConflict, "cannot disable yourself")
	case errors.Is(err, service.ErrInvalidQuota):
		writeError(w, http.StatusBadRequest, "quota_daily must be 0-100000")
	case errors.Is(err, service.ErrReasonTooLong):
		writeError(w, http.StatusBadRequest, "reason must be at most 500 bytes")
	default:
		writeError(w, http.StatusInternalServerError, "admin request failed")
	}
}

// --- handlers ---

// Me handles GET /api/admin/me
func (h *AdminHandler) Me(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.Me(r.Context(), actor(r))
	if err != nil {
		writeAdminError(w, err, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": idString(u.ID), "email": u.Email, "role": u.Role})
}

// ListUsers handles GET /api/admin/users
func (h *AdminHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, err := parseIDParam(q.Get("cursor"), "cursor")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	status, err := parseEnum(q.Get("status"), "status", "active", "disabled")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	users, err := h.svc.ListUsers(r.Context(), service.UserFilter{Query: q.Get("q"), Status: status, BeforeID: before})
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	out := make([]map[string]any, len(users))
	for i, u := range users {
		out[i] = adminUserJSON(u, false)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users":       out,
		"next_cursor": nextCursor(len(users), func() int64 { return users[len(users)-1].ID }),
	})
}

// userAction parses {id}, runs fn and writes the user detail.
func (h *AdminHandler) userAction(w http.ResponseWriter, r *http.Request, fn func(id int64) (*service.AdminUser, error)) {
	id, err := parseIDParam(r.PathValue("id"), "id")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	u, err := fn(id)
	if err != nil {
		writeAdminError(w, err, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, adminUserJSON(u, true))
}

// GetUser handles GET /api/admin/users/{id}
func (h *AdminHandler) GetUser(w http.ResponseWriter, r *http.Request) {
	h.userAction(w, r, func(id int64) (*service.AdminUser, error) { return h.svc.GetUser(r.Context(), id) })
}

// PatchUser handles PATCH /api/admin/users/{id}
func (h *AdminHandler) PatchUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		QuotaDaily *int   `json:"quota_daily"`
		Reason     string `json:"reason"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	if body.QuotaDaily == nil {
		writeError(w, http.StatusBadRequest, "quota_daily is required")
		return
	}
	h.userAction(w, r, func(id int64) (*service.AdminUser, error) {
		return h.svc.SetQuota(r.Context(), actor(r), id, *body.QuotaDaily, body.Reason)
	})
}

type reasonBody struct {
	Reason string `json:"reason"`
}

// DisableUser handles POST /api/admin/users/{id}/disable
func (h *AdminHandler) DisableUser(w http.ResponseWriter, r *http.Request) {
	var body reasonBody
	if !decodeOptional(w, r, &body) {
		return
	}
	h.userAction(w, r, func(id int64) (*service.AdminUser, error) {
		return h.svc.DisableUser(r.Context(), actor(r), id, body.Reason)
	})
}

// EnableUser handles POST /api/admin/users/{id}/enable
func (h *AdminHandler) EnableUser(w http.ResponseWriter, r *http.Request) {
	var body reasonBody
	if !decodeOptional(w, r, &body) {
		return
	}
	h.userAction(w, r, func(id int64) (*service.AdminUser, error) {
		return h.svc.EnableUser(r.Context(), actor(r), id, body.Reason)
	})
}

// ListLinks handles GET /api/admin/links
func (h *AdminHandler) ListLinks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, err := parseIDParam(q.Get("cursor"), "cursor")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	userID, err := parseIDParam(q.Get("user_id"), "user_id")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	status, err := parseEnum(q.Get("status"), "status", "active", "disabled", "expired")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	links, err := h.svc.ListLinks(r.Context(), service.LinkFilter{Query: q.Get("q"), Status: status, UserID: userID, BeforeID: before})
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	out := make([]map[string]any, len(links))
	for i, l := range links {
		out[i] = h.adminLinkJSON(l)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"links":       out,
		"next_cursor": nextCursor(len(links), func() int64 { return links[len(links)-1].ID }),
	})
}

// DisableLink handles POST /api/admin/links/{code}/disable
func (h *AdminHandler) DisableLink(w http.ResponseWriter, r *http.Request) {
	h.linkAction(w, r, h.svc.DisableLink)
}

// EnableLink handles POST /api/admin/links/{code}/enable
func (h *AdminHandler) EnableLink(w http.ResponseWriter, r *http.Request) {
	h.linkAction(w, r, h.svc.EnableLink)
}

func (h *AdminHandler) linkAction(w http.ResponseWriter, r *http.Request,
	fn func(ctx context.Context, actorID int64, code, reason string) (*service.AdminLink, error)) {
	var body reasonBody
	if !decodeOptional(w, r, &body) {
		return
	}
	link, err := fn(r.Context(), actor(r), r.PathValue("code"), body.Reason)
	if err != nil {
		writeAdminError(w, err, "link not found")
		return
	}
	writeJSON(w, http.StatusOK, h.adminLinkJSON(link))
}

// DeleteLink handles DELETE /api/admin/links/{code}
func (h *AdminHandler) DeleteLink(w http.ResponseWriter, r *http.Request) {
	var body reasonBody
	if !decodeOptional(w, r, &body) {
		return
	}
	if err := h.svc.DeleteLink(r.Context(), actor(r), r.PathValue("code"), body.Reason); err != nil {
		writeAdminError(w, err, "link not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListAudit handles GET /api/admin/audit
func (h *AdminHandler) ListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, err := parseIDParam(q.Get("cursor"), "cursor")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	actorID, err := parseIDParam(q.Get("actor_id"), "actor_id")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	resourceType, err := parseEnum(q.Get("resource_type"), "resource_type", "user", "link")
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	entries, err := h.svc.ListAudit(r.Context(), service.AuditFilter{ActorID: actorID, ResourceType: resourceType, BeforeID: before})
	if err != nil {
		writeAdminError(w, err, "")
		return
	}
	out := make([]map[string]any, len(entries))
	for i, e := range entries {
		out[i] = auditJSON(e)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":     out,
		"next_cursor": nextCursor(len(entries), func() int64 { return entries[len(entries)-1].ID }),
	})
}

