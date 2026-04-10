package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/april/shorten-link/internal/service"
)

type userMySQL struct {
	db *sqlx.DB
}

// NewUserMySQL returns a UserRepository backed by MySQL.
func NewUserMySQL(db *sqlx.DB) service.UserRepository {
	return &userMySQL{db: db}
}

func (r *userMySQL) Create(ctx context.Context, user *service.User) error {
	const q = `
		INSERT INTO users (id, email, password_hash, plan, quota_daily)
		VALUES (:id, :email, :password_hash, :plan, :quota_daily)`
	_, err := r.db.NamedExecContext(ctx, q, map[string]any{
		"id":            user.ID,
		"email":         user.Email,
		"password_hash": user.PasswordHash,
		"plan":          user.Plan,
		"quota_daily":   user.QuotaDaily,
	})
	if err != nil {
		return fmt.Errorf("repository: create user: %w", err)
	}
	return nil
}

func (r *userMySQL) GetByEmail(ctx context.Context, email string) (*service.User, error) {
	const q = `SELECT id, email, password_hash, plan, quota_daily FROM users WHERE email = ?`
	var row struct {
		ID           int64  `db:"id"`
		Email        string `db:"email"`
		PasswordHash string `db:"password_hash"`
		Plan         string `db:"plan"`
		QuotaDaily   int    `db:"quota_daily"`
	}
	if err := r.db.GetContext(ctx, &row, q, email); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrNotFound
		}
		return nil, fmt.Errorf("repository: get user by email: %w", err)
	}
	return &service.User{
		ID:           row.ID,
		Email:        row.Email,
		PasswordHash: row.PasswordHash,
		Plan:         row.Plan,
		QuotaDaily:   row.QuotaDaily,
	}, nil
}

func (r *userMySQL) GetByID(ctx context.Context, id int64) (*service.User, error) {
	const q = `SELECT id, email, password_hash, plan, quota_daily FROM users WHERE id = ?`
	var row struct {
		ID           int64  `db:"id"`
		Email        string `db:"email"`
		PasswordHash string `db:"password_hash"`
		Plan         string `db:"plan"`
		QuotaDaily   int    `db:"quota_daily"`
	}
	if err := r.db.GetContext(ctx, &row, q, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrNotFound
		}
		return nil, fmt.Errorf("repository: get user by id: %w", err)
	}
	return &service.User{
		ID:           row.ID,
		Email:        row.Email,
		PasswordHash: row.PasswordHash,
		Plan:         row.Plan,
		QuotaDaily:   row.QuotaDaily,
	}, nil
}
