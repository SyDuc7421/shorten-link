package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/service"
)

type scanRepo struct {
	service.NotificationRepository
	mu        sync.Mutex
	links     []service.ExpiringLink
	calls     int
	gotWindow time.Duration
	gotLimit  int
}

func (r *scanRepo) ExpiringLinks(_ context.Context, _ time.Time, window time.Duration, limit int) ([]service.ExpiringLink, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.gotWindow, r.gotLimit = window, limit
	return r.links, nil
}

func (r *scanRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type publishedEvent struct {
	key string
	ev  service.NotificationEvent
}

type recordingPub struct {
	mu      sync.Mutex
	got     []publishedEvent
	failFor map[int64]bool // link ids whose publish fails
}

func (p *recordingPub) Publish(_ context.Context, key string, payload any) error {
	ev := payload.(service.NotificationEvent)
	if p.failFor[ev.LinkID] {
		return errors.New("broker down")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, publishedEvent{key, ev})
	return nil
}

func twoLinks() []service.ExpiringLink {
	exp := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	return []service.ExpiringLink{
		{ID: 11, UserID: 1, ShortCode: "aaa", ExpiresAt: exp},
		{ID: 22, UserID: 2, ShortCode: "bbb", ExpiresAt: exp.Add(time.Hour)},
	}
}

func TestExpiryScanner_PublishesEachLink(t *testing.T) {
	repo := &scanRepo{links: twoLinks()}
	pub := &recordingPub{}
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	n, err := service.NewExpiryScanner(repo, pub, zap.NewNop()).ScanOnce(context.Background(), now)
	if err != nil || n != 2 {
		t.Fatalf("ScanOnce = %d, %v; want 2, nil", n, err)
	}
	if repo.gotWindow != service.ExpiryWarningWindow || repo.gotLimit != 500 {
		t.Errorf("repo called with window %v limit %d", repo.gotWindow, repo.gotLimit)
	}
	if len(pub.got) != 2 {
		t.Fatalf("published %d events, want 2", len(pub.got))
	}
	for i, l := range repo.links {
		g := pub.got[i]
		wantKey := map[int64]string{11: "expiring:11", 22: "expiring:22"}[l.ID]
		if g.key != service.RouteLinkExpiring || g.ev.DedupeKey != wantKey ||
			g.ev.UserID != l.UserID || g.ev.LinkID != l.ID || g.ev.ShortCode != l.ShortCode ||
			g.ev.ExpiresAt == nil || !g.ev.ExpiresAt.Equal(l.ExpiresAt) || !g.ev.OccurredAt.Equal(now) {
			t.Errorf("event %d = %+v", i, g)
		}
	}
}

func TestExpiryScanner_PublishFailureSkips(t *testing.T) {
	repo := &scanRepo{links: twoLinks()}
	pub := &recordingPub{failFor: map[int64]bool{11: true}}

	n, err := service.NewExpiryScanner(repo, pub, zap.NewNop()).ScanOnce(context.Background(), time.Now())
	if err != nil || n != 1 {
		t.Fatalf("ScanOnce = %d, %v; want 1, nil", n, err)
	}
	if len(pub.got) != 1 || pub.got[0].ev.LinkID != 22 {
		t.Errorf("published %+v, want only link 22", pub.got)
	}
}

func TestExpiryScanner_RunStopsOnCancel(t *testing.T) {
	repo := &scanRepo{}
	s := service.NewExpiryScanner(repo, &recordingPub{}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 10*time.Millisecond) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if repo.callCount() < 1 {
		t.Error("Run never scanned")
	}
}
