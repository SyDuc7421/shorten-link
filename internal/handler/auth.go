package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/april/shorten-link/internal/service"
)

// AuthServicer is the subset of AuthService used by the auth handler.
type AuthServicer interface {
	Register(ctx context.Context, email, password string) (*service.TokenPair, error)
	Login(ctx context.Context, email, password string) (*service.TokenPair, error)
	Refresh(ctx context.Context, refreshToken string) (*service.TokenPair, error)
}

// AuthHandler holds the HTTP handlers for auth operations.
type AuthHandler struct {
	svc AuthServicer
}

// NewAuthHandler constructs an AuthHandler.
func NewAuthHandler(svc AuthServicer) *AuthHandler {
	return &AuthHandler{svc: svc}
}

// Register mounts auth routes onto mux.
func (h *AuthHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/register", h.RegisterUser)
	mux.HandleFunc("POST /api/auth/login", h.Login)
	mux.HandleFunc("POST /api/auth/refresh", h.Refresh)
}

// RegisterUser handles POST /api/auth/register
func (h *AuthHandler) RegisterUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	pair, err := h.svc.Register(r.Context(), body.Email, body.Password)
	if errors.Is(err, service.ErrEmailTaken) {
		writeError(w, http.StatusConflict, "email already registered")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "registration failed")
		return
	}

	writeJSON(w, http.StatusCreated, tokenResponse(pair))
}

// Login handles POST /api/auth/login
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	pair, err := h.svc.Login(r.Context(), body.Email, body.Password)
	if errors.Is(err, service.ErrInvalidCredentials) {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if errors.Is(err, service.ErrAccountLocked) {
		writeError(w, http.StatusTooManyRequests, "account temporarily locked due to too many failed attempts")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	writeJSON(w, http.StatusOK, tokenResponse(pair))
}

// Refresh handles POST /api/auth/refresh
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "refresh_token is required")
		return
	}

	pair, err := h.svc.Refresh(r.Context(), body.RefreshToken)
	if errors.Is(err, service.ErrInvalidToken) {
		writeError(w, http.StatusUnauthorized, "invalid or expired refresh token")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "token refresh failed")
		return
	}

	writeJSON(w, http.StatusOK, tokenResponse(pair))
}

func tokenResponse(pair *service.TokenPair) map[string]string {
	return map[string]string{
		"access_token":  pair.AccessToken,
		"refresh_token": pair.RefreshToken,
	}
}
