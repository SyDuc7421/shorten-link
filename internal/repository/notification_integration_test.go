package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/april/shorten-link/internal/repository"
	"github.com/april/shorten-link/internal/service"
)

func ptr[T any](v T) *T { return &v }

func insertNotif(t *testing.T, repo service.NotificationRepository, userID int64, key string) {
	t.Helper()
	n := &service.Notification{UserID: userID, Type: service.NotificationLinkDisabled, LinkID: ptr(int64(7)), ShortCode: ptr("abc")}
	if err := repo.Insert(context.Background(), n, key); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationMySQL_InsertDedupe(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewNotificationMySQL(db)
	ctx := context.Background()
	insertNotif(t, repo, 1, "disabled:x")
	insertNotif(t, repo, 1, "disabled:x")
	got, err := repo.ListByUser(ctx, 1, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
}

func TestNotificationMySQL_ListNewestFirstAndPaging(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewNotificationMySQL(db)
	ctx := context.Background()
	insertNotif(t, repo, 1, "a")
	insertNotif(t, repo, 1, "b")
	insertNotif(t, repo, 2, "c")
	insertNotif(t, repo, 1, "d")

	page1, err := repo.ListByUser(ctx, 1, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].ID <= page1[1].ID {
		t.Fatalf("page1 = %+v, want 2 rows with ids descending", page1)
	}
	page2, err := repo.ListByUser(ctx, 1, page1[1].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].ID >= page1[1].ID {
		t.Fatalf("page2 = %+v, want 1 older row", page2)
	}
	for _, n := range append(page1, page2...) {
		if n.UserID != 1 {
			t.Fatalf("row of user %d leaked into user 1's list", n.UserID)
		}
	}
}

func TestNotificationMySQL_PasswordChangedHasNullLink(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewNotificationMySQL(db)
	ctx := context.Background()
	if err := repo.Insert(ctx, &service.Notification{UserID: 1, Type: service.NotificationPasswordChanged}, "password:x"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.ListByUser(ctx, 1, 0, 20)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, err %v", got, err)
	}
	if got[0].LinkID != nil || got[0].ShortCode != nil || got[0].ExpiresAt != nil {
		t.Fatalf("want nil link fields, got %+v", got[0])
	}
}

func TestNotificationMySQL_MarkRead(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewNotificationMySQL(db)
	ctx := context.Background()
	insertNotif(t, repo, 1, "a")
	insertNotif(t, repo, 1, "b")
	rows, _ := repo.ListByUser(ctx, 1, 0, 20)
	id := rows[0].ID

	if err := repo.MarkRead(ctx, 2, id); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("other user's id: err = %v, want ErrNotFound", err)
	}
	if n, _ := repo.CountUnread(ctx, 1); n != 2 {
		t.Fatalf("unread = %d, want 2 (row must be unchanged)", n)
	}
	if err := repo.MarkRead(ctx, 1, 999999); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("missing id: err = %v, want ErrNotFound", err)
	}
	if err := repo.MarkRead(ctx, 1, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := repo.CountUnread(ctx, 1); n != 1 {
		t.Fatalf("unread = %d, want 1", n)
	}
	first := readAt(t, db, id)
	if first == nil {
		t.Fatal("read_at not set")
	}
	time.Sleep(1100 * time.Millisecond)
	if err := repo.MarkRead(ctx, 1, id); err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if second := readAt(t, db, id); second == nil || !second.Equal(*first) {
		t.Fatalf("read_at changed: %v -> %v", first, second)
	}
}

func readAt(t *testing.T, db *sqlx.DB, id int64) *time.Time {
	t.Helper()
	var v *time.Time
	if err := db.Get(&v, `SELECT read_at FROM notifications WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNotificationMySQL_MarkAllRead(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewNotificationMySQL(db)
	ctx := context.Background()
	insertNotif(t, repo, 1, "a")
	insertNotif(t, repo, 1, "b")
	insertNotif(t, repo, 2, "c")
	if err := repo.MarkAllRead(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if n, _ := repo.CountUnread(ctx, 1); n != 0 {
		t.Fatalf("user 1 unread = %d, want 0", n)
	}
	if n, _ := repo.CountUnread(ctx, 2); n != 1 {
		t.Fatalf("user 2 unread = %d, want 1", n)
	}
}

func TestNotificationMySQL_ExpiringLinks(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewNotificationMySQL(db)
	ctx := context.Background()
	now := time.Date(2030, 1, 10, 12, 0, 0, 0, time.UTC)

	for _, u := range []struct {
		id       int64
		disabled *time.Time
	}{{1, nil}, {2, ptr(now)}} {
		mustExec(t, db, `INSERT INTO users (id, email, password_hash, disabled_at) VALUES (?, ?, 'x', ?)`,
			u.id, "u"+string(rune('0'+u.id))+"@example.com", u.disabled)
	}
	link := func(id, userID int64, code string, expires, created time.Time, disabled *time.Time) {
		mustExec(t, db, `INSERT INTO links (id, user_id, short_code, original_url, expires_at, created_at, disabled_at)
			VALUES (?, ?, ?, 'https://example.com', ?, ?, ?)`, id, userID, code, expires, created, disabled)
	}
	in12h := now.Add(12 * time.Hour)
	old := now.Add(-10 * 24 * time.Hour)
	link(1, 1, "A", in12h, old, nil)                      // wanted
	link(2, 1, "B", now.Add(30*time.Hour), old, nil)      // too far
	link(3, 1, "C", now.Add(-time.Hour), old, nil)        // already expired
	link(4, 1, "D", in12h, in12h.Add(-20*time.Hour), nil) // short-lived
	link(5, 1, "E", in12h, old, ptr(now))                 // link disabled
	link(6, 2, "F", in12h, old, nil)                      // owner disabled
	link(7, 1, "G", in12h, old, nil)                      // already warned
	mustExec(t, db, `INSERT INTO notifications (user_id, type, dedupe_key) VALUES (1, 'link_expiring', 'expiring:7')`)

	got, err := repo.ExpiringLinks(ctx, now, service.ExpiryWarningWindow, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 || got[0].UserID != 1 || got[0].ShortCode != "A" || !got[0].ExpiresAt.Equal(in12h) {
		t.Fatalf("got %+v, want only link A", got)
	}
}

func TestNotificationMySQL_InsertSurfacesOtherErrors(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewNotificationMySQL(db)
	n := &service.Notification{UserID: 1, Type: service.NotificationLinkDisabled, LinkID: ptr(int64(7)), ShortCode: ptr("this-code-is-way-too-long")}
	if err := repo.Insert(context.Background(), n, "disabled:long"); err == nil {
		t.Fatal("want error for short_code longer than 16 chars, got nil")
	}
}

func TestNotificationMySQL_InsertStoresGivenCreatedAt(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewNotificationMySQL(db)
	ctx := context.Background()
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	n := &service.Notification{UserID: 1, Type: service.NotificationPasswordChanged, CreatedAt: at}
	if err := repo.Insert(ctx, n, "password:t"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.ListByUser(ctx, 1, 0, 20)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, err %v", got, err)
	}
	if !got[0].CreatedAt.Equal(at) {
		t.Errorf("created_at = %v, want %v", got[0].CreatedAt, at)
	}
}
