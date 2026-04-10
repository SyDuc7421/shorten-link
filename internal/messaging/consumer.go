package messaging

import (
	"context"
	"encoding/json"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/service"
)

const (
	batchSize  = 500
	flushEvery = 2 * time.Second
)

// ClickConsumer drains the clicks queue and batch-updates click counters.
// It flushes when 500 events accumulate OR every 2 seconds, whichever comes first.
type ClickConsumer struct {
	ch   *amqp.Channel
	repo service.LinkRepository
	log  *zap.Logger
}

// NewClickConsumer constructs a ClickConsumer. ch must already have QoS set if desired.
func NewClickConsumer(ch *amqp.Channel, repo service.LinkRepository, log *zap.Logger) *ClickConsumer {
	return &ClickConsumer{ch: ch, repo: repo, log: log}
}

// Start begins consuming from the clicks queue. It blocks until ctx is cancelled
// or the channel is closed, flushing any buffered events before returning.
func (c *ClickConsumer) Start(ctx context.Context) error {
	if err := c.ch.Qos(batchSize, 0, false); err != nil {
		return err
	}

	msgs, err := c.ch.Consume(QueueClicks, "click-consumer", false, false, false, false, nil)
	if err != nil {
		return err
	}

	counts := make(map[string]int64, batchSize)
	pending := make([]amqp.Delivery, 0, batchSize)
	timer := time.NewTimer(flushEvery)
	defer timer.Stop()

	flush := func() {
		if len(counts) == 0 {
			return
		}
		if err := c.repo.BatchUpdateClicks(context.Background(), counts); err != nil {
			c.log.Error("batch update clicks", zap.Error(err))
			for _, d := range pending {
				d.Nack(false, true) //nolint:errcheck // requeue on failure
			}
		} else {
			for _, d := range pending {
				d.Ack(false) //nolint:errcheck
			}
		}
		counts = make(map[string]int64, batchSize)
		pending = pending[:0]
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(flushEvery)
	}

	for {
		select {
		case d, ok := <-msgs:
			if !ok {
				flush()
				return nil
			}
			var evt struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(d.Body, &evt); err != nil || evt.Code == "" {
				c.log.Warn("invalid click event, discarding", zap.ByteString("body", d.Body))
				d.Nack(false, false) //nolint:errcheck
				continue
			}
			counts[evt.Code]++
			pending = append(pending, d)
			if len(pending) >= batchSize {
				flush()
			}

		case <-timer.C:
			flush()

		case <-ctx.Done():
			flush()
			return nil
		}
	}
}

// DLQConsumer logs dead-letter messages and acknowledges them.
type DLQConsumer struct {
	ch  *amqp.Channel
	log *zap.Logger
}

// NewDLQConsumer constructs a DLQConsumer.
func NewDLQConsumer(ch *amqp.Channel, log *zap.Logger) *DLQConsumer {
	return &DLQConsumer{ch: ch, log: log}
}

// Start begins consuming from the DLQ. It blocks until ctx is cancelled or
// the channel is closed.
func (c *DLQConsumer) Start(ctx context.Context) error {
	msgs, err := c.ch.Consume(QueueDLQ, "dlq-consumer", false, false, false, false, nil)
	if err != nil {
		return err
	}

	for {
		select {
		case d, ok := <-msgs:
			if !ok {
				return nil
			}
			c.log.Error("dead-letter message received — manual inspection required",
				zap.ByteString("body", d.Body),
				zap.String("routing_key", d.RoutingKey),
			)
			d.Ack(false) //nolint:errcheck
		case <-ctx.Done():
			return nil
		}
	}
}
