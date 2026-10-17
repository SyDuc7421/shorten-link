package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/april/shorten-link/internal/logger"
	"github.com/april/shorten-link/pkg/idgen"
)

const (
	accessTokenTTL  = 15 * time.Minute
	refreshTokenTTL = 7 * 24 * time.Hour
	bcryptCost      = 12
	lockoutMax      = 5
	lockoutTTL      = 15 * time.Minute
	minPasswordLen  = 8
	maxPasswordLen  = 72 // bcrypt input limit

	resetTokenTTL       = 30 * time.Minute
	resetCooldown       = time.Minute // at most one reset mail per address per minute
	resetMailWindow     = 24 * time.Hour
	resetMailsPerWindow = 5 // and at most 5 a day, whatever the number of IPs
	resetWorkTimeout    = 10 * time.Second
	revokeAttempts      = 3
)

// dummyHash is compared against when the email is unknown so that login
// takes the same time whether or not the account exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcryptCost)

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// AuthService handles user registration, login, and token refresh.
type AuthService struct {
	users      UserRepository
	tokens     TokenCache
	gen        *idgen.Generator
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	mailer     Mailer
	resetURL   string // GUI page; ?token= is appended
}

// NewAuthService constructs an AuthService.
func NewAuthService(
	users UserRepository,
	tokens TokenCache,
	gen *idgen.Generator,
	privateKey *rsa.PrivateKey,
	publicKey *rsa.PublicKey,
	mailer Mailer,
	resetURL string,
) *AuthService {
	return &AuthService{
		users:      users,
		tokens:     tokens,
		gen:        gen,
		privateKey: privateKey,
		publicKey:  publicKey,
		mailer:     mailer,
		resetURL:   resetURL,
	}
}

// Register creates a new user and returns a token pair.
func (s *AuthService) Register(ctx context.Context, email, password string) (*TokenPair, error) {
	email = normalizeEmail(email)
	if len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return nil, ErrWeakPassword
	}

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

	return s.issueTokenPair(ctx, user.ID, time.Now())
}

// Login verifies credentials and returns a token pair.
// Accounts are locked for lockoutTTL after lockoutMax consecutive failures.
func (s *AuthService) Login(ctx context.Context, email, password string) (*TokenPair, error) {
	// Taken before the password check: a reset that lands after this moment
	// must invalidate a session authorised by the old password.
	authorizedAt := time.Now()
	email = normalizeEmail(email)

	failures, err := s.tokens.GetLockout(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("service: login lockout lookup: %w", err)
	}
	if failures >= lockoutMax {
		return nil, ErrAccountLocked
	}

	user, err := s.users.GetByEmail(ctx, email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("service: login lookup: %w", err)
	}

	// Unknown emails still run bcrypt and count toward lockout so they are
	// indistinguishable from wrong passwords by timing or response.
	hash := dummyHash
	if user != nil {
		hash = []byte(user.PasswordHash)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil || user == nil {
		count, _ := s.tokens.IncrLockout(ctx, email, lockoutTTL)
		if count >= lockoutMax {
			return nil, ErrAccountLocked
		}
		return nil, ErrInvalidCredentials
	}

	s.tokens.DeleteLockout(ctx, email) //nolint:errcheck
	pair, err := s.issueTokenPair(ctx, user.ID, authorizedAt)
	if errors.Is(err, ErrInvalidToken) {
		// the password was reset while this login was checking the old one
		return nil, ErrInvalidCredentials
	}
	return pair, err
}

// Refresh validates a refresh token, invalidates it, and issues a new pair.
// Reuse of a consumed token is treated as a compromise signal — the token is
// deleted and ErrInvalidToken is returned so the caller must re-login.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	tokenID, err := s.parseRefreshToken(refreshToken)
	if err != nil {
		return nil, ErrInvalidToken
	}

	// Taken before the consume, so a revoke that runs between the consume and
	// storing the new token blocks it.
	authorizedAt := time.Now()

	// consume atomically (rotation) so concurrent reuse can only succeed once
	userID, err := s.tokens.ConsumeRefreshToken(ctx, tokenID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("service: refresh consume: %w", err)
	}

	return s.issueTokenPair(ctx, userID, authorizedAt)
}

// RequestPasswordReset mails a single-use reset link if email belongs to a
// user. All the work happens in the background, so the call takes the same
// time and has the same outcome for every address, registered or not, and a
// storage failure cannot show up as a different response. Failures are
// logged with the request's logger.
func (s *AuthService) RequestPasswordReset(ctx context.Context, email string) {
	email = normalizeEmail(email)
	log := logger.FromContext(ctx)
	s.runAsync(func(ctx context.Context) {
		if err := s.sendResetLink(ctx, email); err != nil {
			log.Error("password reset request", zap.Error(err))
		}
	})
}

func (s *AuthService) sendResetLink(ctx context.Context, email string) error {
	user, err := s.users.GetByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("service: reset lookup: %w", err)
	}

	ok, err := s.tokens.AcquireResetSlot(ctx, email, resetCooldown, resetMailWindow, resetMailsPerWindow)
	if err != nil {
		return fmt.Errorf("service: reset slot: %w", err)
	}
	if !ok {
		return nil // a mail went out recently, or the daily cap is reached
	}

	token, err := newResetToken()
	if err == nil {
		err = s.tokens.StorePasswordReset(ctx, hashResetToken(token), user.ID, resetTokenTTL)
	}
	if err != nil {
		// No mail was sent, so do not make the user wait out the cooldown.
		s.tokens.ReleaseResetSlot(ctx, email) //nolint:errcheck
		return fmt.Errorf("service: store reset token: %w", err)
	}

	link, err := s.resetLink(token)
	if err != nil {
		return err
	}
	return s.mailer.SendPasswordReset(ctx, email, link)
}

// ResetPassword sets a new password using a token from RequestPasswordReset
// and revokes every refresh token of the user. The password is checked before
// the token is consumed, so a weak password does not burn the link.
func (s *AuthService) ResetPassword(ctx context.Context, token, password string) error {
	if len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return ErrWeakPassword
	}

	userID, err := s.tokens.ConsumePasswordReset(ctx, hashResetToken(token))
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidToken
	}
	if err != nil {
		return fmt.Errorf("service: consume reset token: %w", err)
	}

	// From here on the token is gone. On failure the user asks for a new link;
	// restoring the token would let a failing request be retried forever.
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("service: reset load user: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("service: hash password: %w", err)
	}

	// Revoke before the change, so that if the change or the second revoke
	// fails, the existing sessions are already gone. Revoke again after it,
	// so a login with the old password or a refresh that was in flight during
	// the change cannot keep a session (see StoreRefreshToken's authorizedAt).
	if err := s.tokens.RevokeAllRefreshTokens(ctx, userID, time.Now(), refreshTokenTTL); err != nil {
		return fmt.Errorf("service: revoke sessions: %w", err)
	}
	if err := s.users.UpdatePasswordHash(ctx, userID, string(hash)); err != nil {
		return fmt.Errorf("service: update password: %w", err)
	}
	var revokeErr error
	for range revokeAttempts {
		if revokeErr = s.tokens.RevokeAllRefreshTokens(ctx, userID, time.Now(), refreshTokenTTL); revokeErr == nil {
			break
		}
	}
	if revokeErr != nil {
		return fmt.Errorf("service: revoke sessions after password change: %w", revokeErr)
	}

	s.tokens.DeleteLockout(ctx, user.Email)   //nolint:errcheck
	s.tokens.DeletePasswordReset(ctx, userID) //nolint:errcheck
	return nil
}

// newResetToken returns 32 random bytes, base64url-encoded without padding.
func newResetToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashResetToken is what is stored, so a copy of Redis cannot be replayed.
func hashResetToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *AuthService) resetLink(token string) (string, error) {
	u, err := url.Parse(s.resetURL)
	if err != nil {
		return "", fmt.Errorf("service: parse reset URL: %w", err)
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// runAsync runs work in the background with its own timeout, which bounds how
// long the goroutine can live if Redis, MySQL or the mail server stalls.
func (s *AuthService) runAsync(work func(ctx context.Context)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resetWorkTimeout)
		defer cancel()
		work(ctx)
	}()
}

// issueTokenPair creates and stores a new access + refresh token pair.
// authorizedAt is when the credential behind this pair was checked.
func (s *AuthService) issueTokenPair(ctx context.Context, userID int64, authorizedAt time.Time) (*TokenPair, error) {
	accessToken, err := s.signAccessToken(userID)
	if err != nil {
		return nil, fmt.Errorf("service: sign access token: %w", err)
	}

	tokenID := uuid.NewString()
	refreshToken, err := s.signRefreshToken(tokenID)
	if err != nil {
		return nil, fmt.Errorf("service: sign refresh token: %w", err)
	}

	err = s.tokens.StoreRefreshToken(ctx, tokenID, userID, refreshTokenTTL, authorizedAt)
	if errors.Is(err, ErrInvalidToken) {
		// the user's sessions were revoked while this request was in flight
		return nil, ErrInvalidToken
	}
	if err != nil {
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
