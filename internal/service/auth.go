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
}

// TokenCache manages refresh tokens in Redis.
type TokenCache interface {
	StoreRefreshToken(ctx context.Context, tokenID string, userID int64, ttl time.Duration) error
	GetRefreshToken(ctx context.Context, tokenID string) (int64, error)
	DeleteRefreshToken(ctx context.Context, tokenID string) error
}

// TokenPair holds an access + refresh token pair returned on login/register/refresh.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
}
