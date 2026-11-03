package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/april/shorten-link/internal/service"
)

// fakeNotifRepo records the arguments the service passes to the repository.
type fakeNotifRepo struct {
	service.NotificationRepository
	gotUser, gotBefore int64
	gotLimit           int
	rows               []*service.Notification
	unread             int64
	markErr            error
	markedAll          int64
}

func (f *fakeNotifRepo) ListByUser(_ context.Context, userID, beforeID int64, limit int) ([]*service.Notification, error) {
	f.gotUser, f.gotBefore, f.gotLimit = userID, beforeID, limit
	return f.rows, nil
}
func (f *fakeNotifRepo) CountUnread(_ context.Context, _ int64) (int64, error) { return f.unread, nil }
func (f *fakeNotifRepo) MarkRead(_ context.Context, _, _ int64) error          { return f.markErr }
func (f *fakeNotifRepo) MarkAllRead(_ context.Context, userID int64) error {
	f.markedAll = userID
	return nil
}

func TestNotificationService_List(t *testing.T) {
	repo := &fakeNotifRepo{rows: []*service.Notification{{ID: 9, Type: service.NotificationLinkEnabled, CreatedAt: time.Now()}}, unread: 3}
	svc := service.NewNotificationService(repo)

	page, err := svc.List(context.Background(), 7, 50)
	if err != nil {
		t.Fatal(err)
	}
	// one extra row is asked for, to tell whether an older page exists
	if repo.gotUser != 7 || repo.gotBefore != 50 || repo.gotLimit != service.NotificationPageSize+1 {
		t.Errorf("repo args: user=%d before=%d limit=%d", repo.gotUser, repo.gotBefore, repo.gotLimit)
	}
	if len(page.Items) != 1 || page.Unread != 3 || page.HasMore {
		t.Errorf("got %d items, unread %d, more %v", len(page.Items), page.Unread, page.HasMore)
	}
}

func TestNotificationService_ListHasMore(t *testing.T) {
	rows := make([]*service.Notification, service.NotificationPageSize+1)
	for i := range rows {
		rows[i] = &service.Notification{ID: int64(100 - i)}
	}
	svc := service.NewNotificationService(&fakeNotifRepo{rows: rows})
	page, err := svc.List(context.Background(), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || len(page.Items) != service.NotificationPageSize {
		t.Errorf("more=%v items=%d, want true and %d", page.HasMore, len(page.Items), service.NotificationPageSize)
	}

	// exactly one page: no next page
	page, _ = service.NewNotificationService(&fakeNotifRepo{rows: rows[:service.NotificationPageSize]}).List(context.Background(), 7, 0)
	if page.HasMore || len(page.Items) != service.NotificationPageSize {
		t.Errorf("exact page: more=%v items=%d", page.HasMore, len(page.Items))
	}
}

func TestNotificationService_MarkRead(t *testing.T) {
	svc := service.NewNotificationService(&fakeNotifRepo{markErr: service.ErrNotFound})
	if err := svc.MarkRead(context.Background(), 1, 2); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestNotificationService_MarkAllRead(t *testing.T) {
	repo := &fakeNotifRepo{}
	if err := service.NewNotificationService(repo).MarkAllRead(context.Background(), 5); err != nil || repo.markedAll != 5 {
		t.Errorf("err=%v marked=%d", err, repo.markedAll)
	}
}
