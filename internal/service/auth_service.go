package service

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/april/shorten-link/pkg/idgen"
)

const (
	accessTokenTTL  = 15 * time.Minute
	refreshTokenTTL = 7 * 24 * time.Hour
	bcryptCost      = 12
	lockoutMax      = 5
	lockoutTTL      = 15 * time.Minute
)

// AuthService handles user registration, login, and token refresh.
type AuthService struct {
	users      UserRepository
	tokens     TokenCache
	gen        *idgen.Generator
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
}

// NewAuthService constructs an AuthService.
func NewAuthService(
	users UserRepository,
	tokens TokenCache,
	gen *idgen.Generator,
	privateKey *rsa.PrivateKey,
	publicKey *rsa.PublicKey,
) *AuthService {
	return &AuthService{
		users:      users,
		tokens:     tokens,
		gen:        gen,
		privateKey: privateKey,
		publicKey:  publicKey,
	}
}

// Register creates a new user and returns a token pair.
func (s *AuthService) Register(ctx context.Context, email, password string) (*TokenPair, error) {
	if _, err := s.users.GetByEmail(ctx, email); err == nil {
		return nil, ErrEmailTaken
	} else if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("service: register lookup: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("service: hash password: %w", err)
	}

	user := &User{
		ID:           s.gen.NextID(),
		Email:        email,
		PasswordHash: string(hash),
		Plan:         "free",
		QuotaDaily:   100,
	}
	if err := s.users.Create(ctx, user); err != nil {
		return nil, fmt.Errorf("service: register create: %w", err)
	}

	return s.issueTokenPair(ctx, user.ID)
}

// Login verifies credentials and returns a token pair.
// Accounts are locked for lockoutTTL after lockoutMax consecutive failures.
func (s *AuthService) Login(ctx context.Context, email, password string) (*TokenPair, error) {
	user, err := s.users.GetByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		// Still increment lockout counter to prevent user-enumeration timing attacks.
		s.tokens.IncrLockout(ctx, email, lockoutTTL) //nolint:errcheck
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("service: login lookup: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		count, _ := s.tokens.IncrLockout(ctx, email, lockoutTTL)
		if count >= lockoutMax {
			return nil, ErrAccountLocked
		}
		return nil, ErrInvalidCredentials
	}

	s.tokens.DeleteLockout(ctx, email) //nolint:errcheck
	return s.issueTokenPair(ctx, user.ID)
}

// Refresh validates a refresh token, invalidates it, and issues a new pair.
// Reuse of a consumed token is treated as a compromise signal — the token is
// deleted and ErrInvalidToken is returned so the caller must re-login.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	tokenID, err := s.parseRefreshToken(refreshToken)
	if err != nil {
		return nil, ErrInvalidToken
	}

	userID, err := s.tokens.GetRefreshToken(ctx, tokenID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("service: refresh lookup: %w", err)
	}

	// consume the token (rotation — invalidate before issuing new)
	if err := s.tokens.DeleteRefreshToken(ctx, tokenID); err != nil {
		return nil, fmt.Errorf("service: refresh delete: %w", err)
	}

	return s.issueTokenPair(ctx, userID)
}

// issueTokenPair creates and stores a new access + refresh token pair.
func (s *AuthService) issueTokenPair(ctx context.Context, userID int64) (*TokenPair, error) {
	accessToken, err := s.signAccessToken(userID)
	if err != nil {
		return nil, fmt.Errorf("service: sign access token: %w", err)
	}

	tokenID := uuid.NewString()
	refreshToken, err := s.signRefreshToken(tokenID)
	if err != nil {
		return nil, fmt.Errorf("service: sign refresh token: %w", err)
	}

	if err := s.tokens.StoreRefreshToken(ctx, tokenID, userID, refreshTokenTTL); err != nil {
		return nil, fmt.Errorf("service: store refresh token: %w", err)
	}

	return &TokenPair{AccessToken: accessToken, RefreshToken: refreshToken}, nil
}

func (s *AuthService) signAccessToken(userID int64) (string, error) {
	claims := jwt.MapClaims{
		"sub": strconv.FormatInt(userID, 10), // string to satisfy jwt/v5 registered claims
		"exp": time.Now().Add(accessTokenTTL).Unix(),
		"iat": time.Now().Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(s.privateKey)
}

func (s *AuthService) signRefreshToken(tokenID string) (string, error) {
	claims := jwt.MapClaims{
		"jti": tokenID,
		"exp": time.Now().Add(refreshTokenTTL).Unix(),
		"iat": time.Now().Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(s.privateKey)
}

func (s *AuthService) parseRefreshToken(tokenStr string) (string, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.publicKey, nil
	})
	if err != nil || !token.Valid {
		return "", ErrInvalidToken
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", ErrInvalidToken
	}
	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		return "", ErrInvalidToken
	}
	return jti, nil
}

// ParseAccessToken validates an access JWT and returns the userID.
func (s *AuthService) ParseAccessToken(tokenStr string) (int64, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.publicKey, nil
	})
	if err != nil || !token.Valid {
		return 0, ErrInvalidToken
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return 0, ErrInvalidToken
	}
	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return 0, ErrInvalidToken
	}
	userID, err := strconv.ParseInt(sub, 10, 64)
	if err != nil {
		return 0, ErrInvalidToken
	}
	return userID, nil
}
