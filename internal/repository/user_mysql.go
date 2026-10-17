package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"github.com/april/shorten-link/internal/service"
)

const mysqlErrDuplicateEntry = 1062

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
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == mysqlErrDuplicateEntry {
		return service.ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("repository: create user: %w", err)
	}
	return nil
}

func (r *userMySQL) GetByEmail(ctx context.Context, email string) (*service.User, error) {
	const q = `SELECT id, email, password_hash, plan, role, quota_daily, disabled_at FROM users WHERE email = ?`
	var row struct {
		ID           int64      `db:"id"`
		Email        string     `db:"email"`
		PasswordHash string     `db:"password_hash"`
		Plan         string     `db:"plan"`
		Role         string     `db:"role"`
		QuotaDaily   int        `db:"quota_daily"`
		DisabledAt   *time.Time `db:"disabled_at"`
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
		Role:         row.Role,
		QuotaDaily:   row.QuotaDaily,
		DisabledAt:   row.DisabledAt,
	}, nil
}

func (r *userMySQL) GetByID(ctx context.Context, id int64) (*service.User, error) {
	const q = `SELECT id, email, password_hash, plan, role, quota_daily, disabled_at FROM users WHERE id = ?`
	var row struct {
		ID           int64      `db:"id"`
		Email        string     `db:"email"`
		PasswordHash string     `db:"password_hash"`
		Plan         string     `db:"plan"`
		Role         string     `db:"role"`
		QuotaDaily   int        `db:"quota_daily"`
		DisabledAt   *time.Time `db:"disabled_at"`
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
		Role:         row.Role,
		QuotaDaily:   row.QuotaDaily,
		DisabledAt:   row.DisabledAt,
	}, nil
}

func (r *userMySQL) UpdatePasswordHash(ctx context.Context, userID int64, hash string) error {
	const q = `UPDATE users SET password_hash = ? WHERE id = ?`
	if _, err := r.db.ExecContext(ctx, q, hash, userID); err != nil {
		return fmt.Errorf("repository: update password hash: %w", err)
	}
	return nil
}
