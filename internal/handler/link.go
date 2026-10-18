package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/service"
)

// LinkServicer is the subset of LinkService used by handlers.
// Defined here (consumer site) so handlers can be tested with a mock.
type LinkServicer interface {
	Shorten(ctx context.Context, userID int64, rawURL, alias string, expiresAt *time.Time) (*service.Link, error)
	CheckAlias(ctx context.Context, alias string) (available bool, reason string, err error)
	Resolve(ctx context.Context, code string) (string, error)
	Delete(ctx context.Context, code string, userID int64) error
	ListByUser(ctx context.Context, userID int64, afterID int64, limit int) ([]*service.Link, error)
}

// LinkHandler holds the HTTP handlers for link operations.
type LinkHandler struct {
	svc     LinkServicer
	baseURL string // e.g. "https://short.ly" — no trailing slash
}

// NewLinkHandler constructs a LinkHandler.
func NewLinkHandler(svc LinkServicer, baseURL string) *LinkHandler {
	return &LinkHandler{svc: svc, baseURL: strings.TrimRight(baseURL, "/")}
}

// Register mounts routes onto mux.
func (h *LinkHandler) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler) {
	// public
	mux.HandleFunc("GET /{code}", h.Redirect)

	// protected
	mux.Handle("POST /api/links", requireAuth(http.HandlerFunc(h.Shorten)))
	mux.Handle("DELETE /api/links/{code}", requireAuth(http.HandlerFunc(h.Delete)))
	mux.Handle("GET /api/links", requireAuth(http.HandlerFunc(h.List)))
	mux.Handle("GET /api/links/availability", requireAuth(http.HandlerFunc(h.CheckAlias)))
}

// Shorten handles POST /api/links
func (h *LinkHandler) Shorten(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxkey.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body struct {
		URL       string  `json:"url"`
		Alias     string  `json:"alias"`
		ExpiresAt *string `json:"expires_at"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	if writeIfTooLarge(w, err) {
		return
	}
	if err != nil || body.URL == "" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var expiresAt *time.Time
	if body.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "expires_at must be RFC3339")
			return
		}
		expiresAt = &t
	}

	link, err := h.svc.Shorten(r.Context(), userID, body.URL, body.Alias, expiresAt)
	if errors.Is(err, service.ErrInvalidURL) {
		writeError(w, http.StatusBadRequest, "URL must be a valid http(s) URL with a resolvable host")
		return
	}
	if errors.Is(err, service.ErrInvalidExpiry) {
		writeError(w, http.StatusBadRequest, "expires_at must be in the future")
		return
	}
	if errors.Is(err, service.ErrForbiddenURL) {
		writeError(w, http.StatusUnprocessableEntity, "URL targets a forbidden address")
		return
	}
	if errors.Is(err, service.ErrInvalidAlias) {
		writeError(w, http.StatusBadRequest, "alias must be 3-16 characters of A-Z, a-z, 0-9, _ or -, and not reserved")
		return
	}
	if errors.Is(err, service.ErrCodeTaken) {
		writeError(w, http.StatusConflict, "alias already taken")
		return
	}
	if errors.Is(err, service.ErrAccountDisabled) {
		writeError(w, http.StatusForbidden, "account disabled")
		return
	}
	if errors.Is(err, service.ErrQuotaExceeded) {
		// The quota resets at 00:00 UTC; round up so a client never retries early.
		now := time.Now().UTC()
		untilReset := now.Truncate(24 * time.Hour).Add(24 * time.Hour).Sub(now)
		w.Header().Set("Retry-After", strconv.Itoa(int(untilReset.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "daily link quota exceeded")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to shorten URL")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"short_url":  fmt.Sprintf("%s/%s", h.baseURL, link.ShortCode),
		"code":       link.ShortCode,
		"expires_at": link.ExpiresAt,
	})
}

// CheckAlias handles GET /api/links/availability?alias=...
func (h *LinkHandler) CheckAlias(w http.ResponseWriter, r *http.Request) {
	if _, ok := ctxkey.UserID(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	alias := r.URL.Query().Get("alias")
	if alias == "" {
		writeError(w, http.StatusBadRequest, "alias query parameter is required")
		return
	}

	available, reason, err := h.svc.CheckAlias(r.Context(), alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check alias")
		return
	}

	writeJSON(w, http.StatusOK, struct {
		Alias     string `json:"alias"`
		Available bool   `json:"available"`
		Reason    string `json:"reason,omitempty"`
	}{alias, available, reason})
}

// Redirect handles GET /{code}
func (h *LinkHandler) Redirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	rawURL, err := h.svc.Resolve(r.Context(), code)
	if err != nil {
		if errors.Is(err, service.ErrLinkDisabled) {
			writeError(w, http.StatusGone, "link disabled")
			return
		}
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "link not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to resolve link")
		return
	}

	http.Redirect(w, r, rawURL, http.StatusFound)
}

// Delete handles DELETE /api/links/{code}
func (h *LinkHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxkey.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	code := r.PathValue("code")
	if err := h.svc.Delete(r.Context(), code, userID); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "link not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete link")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// List handles GET /api/links
func (h *LinkHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxkey.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var afterID int64
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		afterID = id
	}

	const pageSize = 20
	links, err := h.svc.ListByUser(r.Context(), userID, afterID, pageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list links")
		return
	}

	var nextCursor string
	if len(links) == pageSize {
		nextCursor = strconv.FormatInt(links[len(links)-1].ID, 10)
	}

	type linkResp struct {
		ID          int64      `json:"id"`
		ShortURL    string     `json:"short_url"`
		Code        string     `json:"code"`
		OriginalURL string     `json:"original_url"`
		Clicks      int64      `json:"clicks"`
		ExpiresAt   *time.Time `json:"expires_at"`
		CreatedAt   time.Time  `json:"created_at"`
	}
	resp := make([]linkResp, len(links))
	for i, l := range links {
		resp[i] = linkResp{
			ID:          l.ID,
			ShortURL:    fmt.Sprintf("%s/%s", h.baseURL, l.ShortCode),
			Code:        l.ShortCode,
			OriginalURL: l.OriginalURL,
			Clicks:      l.Clicks,
			ExpiresAt:   l.ExpiresAt,
			CreatedAt:   l.CreatedAt,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"links":       resp,
		"next_cursor": nextCursor,
	})
}

func isNotFound(err error) bool {
	return errors.Is(err, service.ErrNotFound)
}
