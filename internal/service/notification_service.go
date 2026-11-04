package service

import "context"

// NotificationService lets a user read their own notifications.
type NotificationService struct {
	repo NotificationRepository
}

func NewNotificationService(repo NotificationRepository) *NotificationService {
	return &NotificationService{repo: repo}
}

// NotificationPage is one page of a user's notifications.
type NotificationPage struct {
	Items   []*Notification
	HasMore bool  // an older page exists
	Unread  int64 // the user's unread count over all pages
}

// List returns one page (NotificationPageSize) older than beforeID (0 = newest)
// and the user's unread count. It asks for one extra row to tell whether an
// older page exists, so a full last page does not lead to an empty one.
func (s *NotificationService) List(ctx context.Context, userID, beforeID int64) (*NotificationPage, error) {
	items, err := s.repo.ListByUser(ctx, userID, beforeID, NotificationPageSize+1)
	if err != nil {
		return nil, err
	}
	more := len(items) > NotificationPageSize
	if more {
		items = items[:NotificationPageSize]
	}
	unread, err := s.repo.CountUnread(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &NotificationPage{Items: items, HasMore: more, Unread: unread}, nil
}

// MarkRead marks one notification read. ErrNotFound passes through.
func (s *NotificationService) MarkRead(ctx context.Context, userID, id int64) error {
	return s.repo.MarkRead(ctx, userID, id)
}

func (s *NotificationService) MarkAllRead(ctx context.Context, userID int64) error {
	return s.repo.MarkAllRead(ctx, userID)
}
