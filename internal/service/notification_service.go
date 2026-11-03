package service

import "context"

// NotificationService lets a user read their own notifications.
type NotificationService struct {
	repo NotificationRepository
}

func NewNotificationService(repo NotificationRepository) *NotificationService {
	return &NotificationService{repo: repo}
}

// List returns one page (NotificationPageSize) older than beforeID (0 = newest) and the user's unread count.
func (s *NotificationService) List(ctx context.Context, userID, beforeID int64) ([]*Notification, int64, error) {
	items, err := s.repo.ListByUser(ctx, userID, beforeID, NotificationPageSize)
	if err != nil {
		return nil, 0, err
	}
	unread, err := s.repo.CountUnread(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	return items, unread, nil
}

// MarkRead marks one notification read. ErrNotFound passes through.
func (s *NotificationService) MarkRead(ctx context.Context, userID, id int64) error {
	return s.repo.MarkRead(ctx, userID, id)
}

func (s *NotificationService) MarkAllRead(ctx context.Context, userID int64) error {
	return s.repo.MarkAllRead(ctx, userID)
}
