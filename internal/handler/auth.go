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
	RequestPasswordReset(ctx context.Context, email string) error
	ResetPassword(ctx context.Context, token, password string) error
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
	mux.HandleFunc("POST /api/auth/forgot-password", h.ForgotPassword)
	mux.HandleFunc("POST /api/auth/reset-password", h.ResetPassword)
}

// RegisterUser handles POST /api/auth/register
func (h *AuthHandler) RegisterUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	if writeIfTooLarge(w, err) {
		return
	}
	if err != nil || body.Email == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	pair, err := h.svc.Register(r.Context(), body.Email, body.Password)
	if errors.Is(err, service.ErrWeakPassword) {
		writeError(w, http.StatusBadRequest, "password must be 8-72 characters")
		return
	}
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
	err := json.NewDecoder(r.Body).Decode(&body)
	if writeIfTooLarge(w, err) {
		return
	}
	if err != nil || body.Email == "" || body.Password == "" {
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
	err := json.NewDecoder(r.Body).Decode(&body)
	if writeIfTooLarge(w, err) {
		return
	}
	if err != nil || body.RefreshToken == "" {
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

// ForgotPassword handles POST /api/auth/forgot-password. It answers 202 for
// every well-formed request so the response does not reveal whether the email
// is registered.
func (h *AuthHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	if writeIfTooLarge(w, err) {
		return
	}
	if err != nil || body.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	if err := h.svc.RequestPasswordReset(r.Context(), body.Email); err != nil {
		writeError(w, http.StatusInternalServerError, "password reset request failed")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ResetPassword handles POST /api/auth/reset-password
func (h *AuthHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	if writeIfTooLarge(w, err) {
		return
	}
	if err != nil || body.Token == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "token and password are required")
		return
	}

	err = h.svc.ResetPassword(r.Context(), body.Token, body.Password)
	if errors.Is(err, service.ErrWeakPassword) {
		writeError(w, http.StatusBadRequest, "password must be 8-72 characters")
		return
	}
	if errors.Is(err, service.ErrInvalidToken) {
		writeError(w, http.StatusUnauthorized, "invalid or expired reset token")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "password reset failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func tokenResponse(pair *service.TokenPair) map[string]string {
	return map[string]string{
		"access_token":  pair.AccessToken,
		"refresh_token": pair.RefreshToken,
	}
}
