package messaging

import (
	"context"
	"encoding/json"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/service"
)

const notificationPrefetch = 50

// NotificationConsumer drains the notifications queue and stores each event
// as a row in the notifications table.
type NotificationConsumer struct {
	ch   *amqp.Channel
	repo service.NotificationRepository
	log  *zap.Logger
}

// NewNotificationConsumer constructs a NotificationConsumer.
func NewNotificationConsumer(ch *amqp.Channel, repo service.NotificationRepository, log *zap.Logger) *NotificationConsumer {
	return &NotificationConsumer{ch: ch, repo: repo, log: log}
}

// Start consumes the notifications queue. It blocks until ctx is cancelled or
// the channel is closed.
func (c *NotificationConsumer) Start(ctx context.Context) error {
	if err := c.ch.Qos(notificationPrefetch, 0, false); err != nil {
		return err
	}
	msgs, err := c.ch.Consume(QueueNotifications, "notification-consumer", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case d, ok := <-msgs:
			if !ok {
				return nil
			}
			c.handle(ctx, d)
		case <-ctx.Done():
			return nil
		}
	}
}

// handle settles one delivery. Malformed messages are dead-lettered at once;
// insert failures are retried once, then dead-lettered (link.expiring is
// dropped instead, since the scanner republishes it).
func (c *NotificationConsumer) handle(ctx context.Context, d amqp.Delivery) {
	typ, ok := service.NotificationTypeForRoute(d.RoutingKey)
	if !ok {
		c.reject(d, "unknown routing key")
		return
	}
	var evt service.NotificationEvent
	if err := json.Unmarshal(d.Body, &evt); err != nil {
		c.reject(d, "bad json")
		return
	}
	if evt.UserID == 0 || evt.DedupeKey == "" {
		c.reject(d, "missing user_id or dedupe_key")
		return
	}

	n := &service.Notification{UserID: evt.UserID, Type: typ, ExpiresAt: evt.ExpiresAt}
	if !evt.OccurredAt.IsZero() {
		n.CreatedAt = evt.OccurredAt
	}
	if typ != service.NotificationPasswordChanged {
		if evt.LinkID == 0 || evt.ShortCode == "" {
			c.reject(d, "missing link_id or short_code")
			return
		}
		n.LinkID = &evt.LinkID
		n.ShortCode = &evt.ShortCode
	}
	if typ == service.NotificationLinkExpiring && evt.ExpiresAt == nil {
		c.reject(d, "missing expires_at")
		return
	}

	// Insert ignores shutdown: a delivery already taken finishes, so a
	// cancelled ctx cannot fail it and push a redelivery to the DLQ.
	if err := c.repo.Insert(context.WithoutCancel(ctx), n, evt.DedupeKey); err != nil {
		c.log.Error("insert notification", zap.Error(err), zap.String("dedupe_key", evt.DedupeKey))
		// The expiry scanner publishes link.expiring again on its next scan
		// while no row exists, so a second failure drops the event instead of
		// adding a copy to the DLQ every scan.
		if d.Redelivered && typ == service.NotificationLinkExpiring {
			d.Ack(false) //nolint:errcheck
			return
		}
		d.Nack(false, !d.Redelivered) //nolint:errcheck
		return
	}
	d.Ack(false) //nolint:errcheck
}

func (c *NotificationConsumer) reject(d amqp.Delivery, reason string) {
	c.log.Warn("invalid notification event, discarding",
		zap.String("reason", reason),
		zap.String("routing_key", d.RoutingKey),
		zap.ByteString("body", d.Body),
	)
	d.Nack(false, false) //nolint:errcheck
}
