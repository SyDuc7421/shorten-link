package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrSelfDisable is returned when an admin tries to disable their own account.
var ErrSelfDisable = errors.New("service: cannot disable yourself")

// ErrInvalidQuota is returned when quota_daily is outside 0..MaxQuotaDaily.
var ErrInvalidQuota = errors.New("service: quota_daily must be 0-100000")

// ErrReasonTooLong is returned when an admin's reason exceeds MaxReasonBytes.
var ErrReasonTooLong = errors.New("service: reason must be at most 500 bytes")

// ErrInvalidDays is returned when the stats range is outside 1..MaxStatsDays.
var ErrInvalidDays = errors.New("service: days must be 1-90")

const (
	MaxStatsDays     = 90
	DefaultStatsDays = 30
	TopLinksLimit    = 10
)

const (
	MaxQuotaDaily  = 100000
	MaxReasonBytes = 500
	// AdminPageSize is the number of rows per page in every admin list.
	AdminPageSize = 20
)

// AdminUser is a user as an admin sees it.
type AdminUser struct {
	ID          int64
	Email       string
	Role        string
	QuotaDaily  int
	DisabledAt  *time.Time
	CreatedAt   time.Time
	LinkCount   int64
	LinksToday  int   // only filled by GetUser
	TotalClicks int64 // only filled by GetUser
}

// AdminLink is a link with its owner, as an admin sees it.
type AdminLink struct {
	ID              int64
	ShortCode       string
	OriginalURL     string
	Clicks          int64
	DisabledAt      *time.Time
	ExpiresAt       *time.Time
	CreatedAt       time.Time
	OwnerID         int64
	OwnerEmail      string
	OwnerDisabledAt *time.Time
}

// AuditEntry is one row of audit_log with the actor's email.
type AuditEntry struct {
	ID           int64
	ActorID      int64  // 0 = system (CLI)
	ActorEmail   string // "" for system
	Action       string
	ResourceType string
	ResourceID   int64
	Metadata     json.RawMessage
	CreatedAt    time.Time
}

// UserFilter selects users for ListUsers.
type UserFilter struct {
	Query    string // email contains
	Status   string // "", "active", "disabled"
	BeforeID int64  // 0 = first page
}

// LinkFilter selects links for ListLinks.
type LinkFilter struct {
	Query    string // code equals or URL contains
	Status   string // "", "active", "disabled", "expired"
	UserID   int64  // 0 = any
	BeforeID int64
}

// AuditFilter selects audit entries for ListAudit.
type AuditFilter struct {
	ActorID      int64  // 0 = any
	ResourceType string // "", "user", "link"
	BeforeID     int64
}

// StatsTotals are counts over the whole database.
type StatsTotals struct {
	Users, Links, Clicks, DisabledUsers, DisabledLinks int64
}

// DayStats is activity on one UTC day.
type DayStats struct {
	Date                       time.Time // UTC midnight
	NewUsers, NewLinks, Clicks int64
}

// TopLink is a link and its clicks within the stats range.
type TopLink struct {
	Code   string
	Clicks int64
}

// Stats is what the admin dashboard shows.
type Stats struct {
	Totals   StatsTotals
	Daily    []DayStats // repository: only days with activity; AdminService: every day, oldest first
	TopLinks []TopLink
}

// AdminRepository is the persistence interface for admin operations. Every
// write runs in one transaction with its audit_log row; changed=false means the
// row was already in that state and nothing (no audit row) was written.
type AdminRepository interface {
	ListUsers(ctx context.Context, f UserFilter) ([]*AdminUser, error)
	GetUser(ctx context.Context, id, todaySinceID int64) (*AdminUser, error)
	SetUserQuota(ctx context.Context, actorID, userID int64, quota int, reason string) error
	SetUserDisabled(ctx context.Context, actorID, userID int64, disabled bool, reason string) (changed bool, err error)
	LinkCodesByUser(ctx context.Context, userID int64) ([]string, error)
	ListLinks(ctx context.Context, f LinkFilter) ([]*AdminLink, error)
	GetLink(ctx context.Context, code string) (*AdminLink, error)
	SetLinkDisabled(ctx context.Context, actorID int64, code string, disabled bool, reason string) (changed bool, err error)
	DeleteLink(ctx context.Context, actorID int64, code string, reason string) error
	ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error)
	SetRole(ctx context.Context, actorID int64, email, role string) (changed bool, err error)
	// Stats returns totals, per-day rows on or after since (UTC midnight) and
	// the top links by clicks in that range.
	Stats(ctx context.Context, since time.Time) (*Stats, error)
}
