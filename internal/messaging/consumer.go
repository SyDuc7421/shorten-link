package messaging

import (
	"context"
	"encoding/json"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/metrics"
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

	batch := newClickBatch()
	timer := time.NewTimer(flushEvery)
	defer timer.Stop()

	flush := func() {
		batch.flush(context.Background(), c.repo, c.log)
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
				metrics.WorkerMessage(QueueClicks, "dlq", 1)
				continue
			}
			batch.add(d, evt.Code)
			if len(batch.pending) >= batchSize {
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

// clickBatch accumulates click deliveries and their per-code counts.
type clickBatch struct {
	counts  map[string]int64
	pending []amqp.Delivery
}

func newClickBatch() *clickBatch {
	return &clickBatch{counts: make(map[string]int64, batchSize), pending: make([]amqp.Delivery, 0, batchSize)}
}

func (b *clickBatch) add(d amqp.Delivery, code string) {
	b.counts[code]++
	b.pending = append(b.pending, d)
}

// flush writes the counts and acks the deliveries, or nacks them on failure.
func (b *clickBatch) flush(ctx context.Context, repo service.LinkRepository, log *zap.Logger) {
	if len(b.counts) == 0 {
		return
	}
	start := time.Now()
	err := repo.BatchUpdateClicks(ctx, b.counts)
	flushDur := time.Since(start)
	metrics.ClickFlush(len(b.pending), flushDur, err == nil)
	if err != nil {
		log.Error("batch update clicks", zap.Error(err), zap.Strings("request_ids", b.requestIDs()))
		// Retry once; a message that already failed is dead-lettered so a
		// persistent DB error cannot loop the same batch forever.
		for _, d := range b.pending {
			d.Nack(false, !d.Redelivered) //nolint:errcheck
			metrics.WorkerMessage(QueueClicks, nackResult(d), 1)
		}
	} else {
		for _, d := range b.pending {
			d.Ack(false) //nolint:errcheck
		}
		metrics.WorkerMessage(QueueClicks, "ack", len(b.pending))
	}
	b.counts = make(map[string]int64, batchSize)
	b.pending = b.pending[:0]
}

// requestIDs lists the request IDs of the pending deliveries (non-empty only,
// at most batchSize) so a failed batch can be traced back to its requests.
func (b *clickBatch) requestIDs() []string {
	var ids []string
	for _, d := range b.pending {
		if id := requestIDOf(d); id != "" && len(ids) < batchSize {
			ids = append(ids, id)
		}
	}
	return ids
}

// nackResult is the worker_messages result for a delivery nacked with
// requeue=!d.Redelivered: "nack" when it goes back to the queue, "dlq" when
// it is dead-lettered.
func nackResult(d amqp.Delivery) string {
	if d.Redelivered {
		return "dlq"
	}
	return "nack"
}

// DLQConsumer logs dead-letter messages and acknowledges them.
type DLQConsumer struct {
	ch    *amqp.Channel
	queue string
	log   *zap.Logger
}

// NewDLQConsumer constructs a DLQConsumer for the given dead-letter queue.
func NewDLQConsumer(ch *amqp.Channel, queue string, log *zap.Logger) *DLQConsumer {
	return &DLQConsumer{ch: ch, queue: queue, log: log}
}

// Start begins consuming from its dead-letter queue. It blocks until ctx is cancelled or
// the channel is closed.
func (c *DLQConsumer) Start(ctx context.Context) error {
	msgs, err := c.ch.Consume(c.queue, c.queue+"-consumer", false, false, false, false, nil)
	if err != nil {
		return err
	}

	for {
		select {
		case d, ok := <-msgs:
			if !ok {
				return nil
			}
			c.handle(d)
		case <-ctx.Done():
			return nil
		}
	}
}

// handle logs one dead-letter delivery and acknowledges it.
func (c *DLQConsumer) handle(d amqp.Delivery) {
	c.log.Error("dead-letter message received — manual inspection required",
		zap.ByteString("body", d.Body),
		zap.String("routing_key", d.RoutingKey),
		ridField(requestIDOf(d)),
	)
	d.Ack(false) //nolint:errcheck
}
