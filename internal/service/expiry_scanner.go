package service

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"
)

const expiryScanLimit = 500

// ExpiryScanner finds links that expire within ExpiryWarningWindow and
// publishes a link.expiring event for each one. The worker's notification
// consumer turns the events into notification rows.
type ExpiryScanner struct {
	repo NotificationRepository
	pub  Publisher
	log  *zap.Logger
}

func NewExpiryScanner(repo NotificationRepository, pub Publisher, log *zap.Logger) *ExpiryScanner {
	return &ExpiryScanner{repo: repo, pub: pub, log: log}
}

// ScanOnce publishes link.expiring for each link ExpiringLinks returns (limit 500)
// and reports how many publishes succeeded. A failed publish is logged and
// skipped; the next scan retries it, since no notification row exists yet.
func (s *ExpiryScanner) ScanOnce(ctx context.Context, now time.Time) (published int, err error) {
	links, err := s.repo.ExpiringLinks(ctx, now, ExpiryWarningWindow, expiryScanLimit)
	if err != nil {
		return 0, err
	}
	for _, l := range links {
		exp := l.ExpiresAt
		ev := NotificationEvent{
			DedupeKey:  "expiring:" + strconv.FormatInt(l.ID, 10),
			UserID:     l.UserID,
			LinkID:     l.ID,
			ShortCode:  l.ShortCode,
			ExpiresAt:  &exp,
			OccurredAt: now,
		}
		pctx, cancel := context.WithTimeout(ctx, publishTimeout)
		perr := s.pub.Publish(pctx, RouteLinkExpiring, ev)
		cancel()
		if perr != nil {
			s.log.Warn("publish link.expiring failed", zap.Int64("link_id", l.ID), zap.Error(perr))
			continue
		}
		published++
	}
	return published, nil
}

// Run calls ScanOnce right away and then on every tick, until ctx is done; then it returns nil.
// An interval of zero or less is an error, returned at once.
// A ScanOnce error is logged, not returned, so a DB blip does not stop the worker.
func (s *ExpiryScanner) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("expiry scanner: interval must be > 0, got %s", interval)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := s.ScanOnce(ctx, time.Now().UTC())
		if err != nil {
			s.log.Error("expiry scan failed", zap.Error(err))
		} else if n > 0 {
			s.log.Info("expiry scan published events", zap.Int("count", n))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
