package service

import (
	"context"
	"errors"
	"time"
)

// ErrEmailTaken is returned when registering with an already-used email.
var ErrEmailTaken = errors.New("service: email already registered")

// ErrInvalidCredentials is returned on login failure.
var ErrInvalidCredentials = errors.New("service: invalid email or password")

// ErrInvalidToken is returned when a JWT or refresh token is invalid/expired.
var ErrInvalidToken = errors.New("service: invalid or expired token")

// ErrAccountLocked is returned after too many consecutive failed login attempts.
var ErrAccountLocked = errors.New("service: account temporarily locked")

// ErrWeakPassword is returned when a password is shorter than 8 or longer than 72 bytes.
var ErrWeakPassword = errors.New("service: password must be 8-72 bytes")

// User is the auth domain model.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	Plan         string
	QuotaDaily   int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserRepository is the persistence interface for users, defined at the consumer site.
type UserRepository interface {
	Create(ctx context.Context, user *User) error
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByID(ctx context.Context, id int64) (*User, error)
	UpdatePasswordHash(ctx context.Context, userID int64, hash string) error
}

// TokenCache manages refresh tokens in Redis.
type TokenCache interface {
	// StoreRefreshToken stores a refresh token. authorizedAt is when the
	// credential that justified it (password or previous refresh token) was
	// checked; if the user's sessions were revoked at or after that moment the
	// token is not stored and ErrInvalidToken is returned.
	StoreRefreshToken(ctx context.Context, tokenID string, userID int64, ttl time.Duration, authorizedAt time.Time) error
	// ConsumeRefreshToken atomically reads and deletes a refresh token.
	ConsumeRefreshToken(ctx context.Context, tokenID string) (int64, error)
	// Lockout methods used by AuthService.
	GetLockout(ctx context.Context, email string) (int64, error)
	IncrLockout(ctx context.Context, email string, ttl time.Duration) (int64, error)
	DeleteLockout(ctx context.Context, email string) error
	// RevokeAllRefreshTokens deletes every refresh token issued to userID and
	// blocks tokens authorised at or before revokedAt from being stored later.
	// ttl must be at least the refresh token lifetime.
	RevokeAllRefreshTokens(ctx context.Context, userID int64, revokedAt time.Time, ttl time.Duration) error
	// Password reset tokens, stored by the SHA-256 of the token. Storing a new
	// one for a user invalidates the previous one.
	StorePasswordReset(ctx context.Context, tokenHash string, userID int64, ttl time.Duration) error
	// ConsumePasswordReset atomically reads and deletes a reset token.
	ConsumePasswordReset(ctx context.Context, tokenHash string) (int64, error)
	DeletePasswordReset(ctx context.Context, userID int64) error
	// AcquireResetCooldown reports true if no reset mail was sent to email within ttl.
	AcquireResetCooldown(ctx context.Context, email string, ttl time.Duration) (bool, error)
}

// Mailer sends account emails, defined at the consumer site. Implemented by internal/mail.
type Mailer interface {
	SendPasswordReset(ctx context.Context, to, link string) error
}

// TokenPair holds an access + refresh token pair returned on login/register/refresh.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
}
