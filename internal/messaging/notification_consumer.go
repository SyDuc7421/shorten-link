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
// insert failures are retried once, then dead-lettered.
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
	if typ != service.NotificationPasswordChanged {
		if evt.LinkID == 0 || evt.ShortCode == "" {
			c.reject(d, "missing link_id or short_code")
			return
		}
		n.LinkID = &evt.LinkID
		n.ShortCode = &evt.ShortCode
	}

	if err := c.repo.Insert(ctx, n, evt.DedupeKey); err != nil {
		c.log.Error("insert notification", zap.Error(err), zap.String("dedupe_key", evt.DedupeKey))
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
