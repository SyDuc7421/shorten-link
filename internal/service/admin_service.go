package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/april/shorten-link/pkg/idgen"
)

// AdminService implements the admin operations: managing users and links and
// reading the audit log. Writes are audited by the repository; this layer
// validates input and keeps sessions and the redirect cache in step.
type AdminService struct {
	repo   AdminRepository
	users  UserRepository
	cache  LinkCache
	tokens TokenCache
}

// NewAdminService constructs an AdminService. cache and tokens may be nil for
// callers that only use SetAdmin (the CLI).
func NewAdminService(repo AdminRepository, users UserRepository, cache LinkCache, tokens TokenCache) *AdminService {
	return &AdminService{repo: repo, users: users, cache: cache, tokens: tokens}
}

func checkReason(reason string) error {
	if len(reason) > MaxReasonBytes {
		return ErrReasonTooLong
	}
	return nil
}

// Me returns the signed-in user.
func (s *AdminService) Me(ctx context.Context, userID int64) (*User, error) {
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("service: admin me: %w", err)
	}
	return u, nil
}

// IsActiveAdmin reports whether userID is an admin whose account is enabled.
// It reads the database every time, so revoking an admin takes effect on
// their next request.
func (s *AdminService) IsActiveAdmin(ctx context.Context, userID int64) (bool, error) {
	u, err := s.users.GetByID(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("service: admin check: %w", err)
	}
	return u.Role == RoleAdmin && u.DisabledAt == nil, nil
}

// --- users ---

func (s *AdminService) ListUsers(ctx context.Context, f UserFilter) ([]*AdminUser, error) {
	users, err := s.repo.ListUsers(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("service: list users: %w", err)
	}
	return users, nil
}

// GetUser returns a user with their link count, links created today (UTC)
// and total clicks.
func (s *AdminService) GetUser(ctx context.Context, id int64) (*AdminUser, error) {
	midnight := time.Now().UTC().Truncate(24 * time.Hour)
	u, err := s.repo.GetUser(ctx, id, idgen.MinIDAt(midnight))
	if err != nil {
		return nil, fmt.Errorf("service: get user: %w", err)
	}
	return u, nil
}

func (s *AdminService) SetQuota(ctx context.Context, actorID, userID int64, quota int, reason string) (*AdminUser, error) {
	if quota < 0 || quota > MaxQuotaDaily {
		return nil, ErrInvalidQuota
	}
	if err := checkReason(reason); err != nil {
		return nil, err
	}
	if err := s.repo.SetUserQuota(ctx, actorID, userID, quota, reason); err != nil {
		return nil, fmt.Errorf("service: set quota: %w", err)
	}
	return s.GetUser(ctx, userID)
}

// DisableUser disables the account, ends all its sessions and stops its links
// redirecting. The session and cache steps run even when the account was
// already disabled, so retrying after a partial failure finishes the job.
func (s *AdminService) DisableUser(ctx context.Context, actorID, userID int64, reason string) (*AdminUser, error) {
	if actorID == userID {
		return nil, ErrSelfDisable
	}
	if err := checkReason(reason); err != nil {
		return nil, err
	}
	if _, err := s.repo.SetUserDisabled(ctx, actorID, userID, true, reason); err != nil {
		return nil, fmt.Errorf("service: disable user: %w", err)
	}
	if err := s.tokens.RevokeAllRefreshTokens(ctx, userID, time.Now(), refreshTokenTTL); err != nil {
		return nil, fmt.Errorf("service: disable user: revoke sessions: %w", err)
	}
	if err := s.purgeUserLinks(ctx, userID); err != nil {
		return nil, fmt.Errorf("service: disable user: %w", err)
	}
	return s.GetUser(ctx, userID)
}

// EnableUser re-enables the account. Its links redirect again, except those
// an admin disabled one by one; the purge removes cached !disabled markers.
func (s *AdminService) EnableUser(ctx context.Context, actorID, userID int64, reason string) (*AdminUser, error) {
	if err := checkReason(reason); err != nil {
		return nil, err
	}
	if _, err := s.repo.SetUserDisabled(ctx, actorID, userID, false, reason); err != nil {
		return nil, fmt.Errorf("service: enable user: %w", err)
	}
	if err := s.purgeUserLinks(ctx, userID); err != nil {
		return nil, fmt.Errorf("service: enable user: %w", err)
	}
	return s.GetUser(ctx, userID)
}

// purgeUserLinks drops the cached redirect of every link the user owns, so the
// next click reads the current disabled state from MySQL.
func (s *AdminService) purgeUserLinks(ctx context.Context, userID int64) error {
	codes, err := s.repo.LinkCodesByUser(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.cache.DeleteMany(ctx, codes); err != nil {
		return fmt.Errorf("purge cache: %w", err)
	}
	return nil
}

// --- links ---

func (s *AdminService) ListLinks(ctx context.Context, f LinkFilter) ([]*AdminLink, error) {
	links, err := s.repo.ListLinks(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("service: list links: %w", err)
	}
	return links, nil
}

func (s *AdminService) DisableLink(ctx context.Context, actorID int64, code, reason string) (*AdminLink, error) {
	return s.setLinkDisabled(ctx, actorID, code, true, reason)
}

func (s *AdminService) EnableLink(ctx context.Context, actorID int64, code, reason string) (*AdminLink, error) {
	return s.setLinkDisabled(ctx, actorID, code, false, reason)
}

func (s *AdminService) setLinkDisabled(ctx context.Context, actorID int64, code string, disabled bool, reason string) (*AdminLink, error) {
	if err := checkReason(reason); err != nil {
		return nil, err
	}
	if _, err := s.repo.SetLinkDisabled(ctx, actorID, code, disabled, reason); err != nil {
		return nil, fmt.Errorf("service: set link disabled: %w", err)
	}
	if err := s.cache.Delete(ctx, code); err != nil {
		return nil, fmt.Errorf("service: set link disabled: purge cache: %w", err)
	}
	link, err := s.repo.GetLink(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("service: set link disabled: %w", err)
	}
	return link, nil
}

func (s *AdminService) DeleteLink(ctx context.Context, actorID int64, code, reason string) error {
	if err := checkReason(reason); err != nil {
		return err
	}
	if err := s.repo.DeleteLink(ctx, actorID, code, reason); err != nil {
		return fmt.Errorf("service: admin delete link: %w", err)
	}
	if err := s.cache.Delete(ctx, code); err != nil {
		return fmt.Errorf("service: admin delete link: purge cache: %w", err)
	}
	return nil
}

// --- audit and roles ---

func (s *AdminService) ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error) {
	entries, err := s.repo.ListAudit(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("service: list audit: %w", err)
	}
	return entries, nil
}

// SetAdmin grants (admin=true) or revokes the admin role by email. Used by the
// CLI; the audit row has actor 0 (system).
func (s *AdminService) SetAdmin(ctx context.Context, email string, admin bool) (bool, error) {
	role := RoleUser
	if admin {
		role = RoleAdmin
	}
	changed, err := s.repo.SetRole(ctx, 0, normalizeEmail(email), role)
	if err != nil {
		return false, fmt.Errorf("service: set admin: %w", err)
	}
	return changed, nil
}
