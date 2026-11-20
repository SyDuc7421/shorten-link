package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/metrics"
)

// reconnectDelay is the pause between reconnect attempts.
var reconnectDelay = 2 * time.Second

// Publisher publishes events to RabbitMQ. It satisfies service.Publisher.
// A background loop re-dials whenever the connection drops, so Publish fails
// fast while the broker is away and recovers without a process restart.
type Publisher struct {
	mu   sync.Mutex
	dial func() (*amqp.Connection, error)
	conn *amqp.Connection
	ch   *amqp.Channel
	log  *zap.Logger

	reconnectDelay time.Duration // copied from the package default at construction

	done      chan struct{}
	closeOnce sync.Once
}

// NewPublisher connects using dial. It does not fail if the broker is
// unreachable; it keeps retrying in the background instead.
func NewPublisher(dial func() (*amqp.Connection, error), log *zap.Logger) *Publisher {
	p := &Publisher{dial: dial, log: log, reconnectDelay: reconnectDelay, done: make(chan struct{})}
	if err := p.connect(); err != nil {
		log.Warn("rabbitmq unavailable, will retry", zap.Error(err))
	}
	go p.reconnectLoop()
	return p
}

// connect dials, declares the topology and opens a channel. The dial happens
// without p.mu held so Publish never waits on network I/O.
func (p *Publisher) connect() error {
	conn, err := p.dial()
	if err != nil {
		return err
	}
	if err := Setup(conn); err != nil {
		conn.Close()
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return err
	}
	p.mu.Lock()
	p.conn, p.ch = conn, ch
	p.mu.Unlock()
	return nil
}

func (p *Publisher) reconnectLoop() {
	for {
		p.mu.Lock()
		conn := p.conn
		p.mu.Unlock()

		if conn != nil && !conn.IsClosed() {
			closed := conn.NotifyClose(make(chan *amqp.Error, 1))
			select {
			case err := <-closed: // nil on a clean close
				p.log.Warn("rabbitmq connection lost", zap.Error(err))
			case <-p.done:
				return
			}
		}

		select {
		case <-time.After(p.reconnectDelay):
		case <-p.done:
			return
		}
		if err := p.connect(); err != nil {
			p.log.Warn("rabbitmq reconnect failed", zap.Error(err))
			continue
		}
		p.log.Info("rabbitmq reconnected")
	}
}

// IsClosed reports whether the publisher currently has no live connection.
func (p *Publisher) IsClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn == nil || p.conn.IsClosed()
}

func (p *Publisher) openChannel() error {
	ch, err := p.conn.Channel()
	if err != nil {
		return fmt.Errorf("publisher: open channel: %w", err)
	}
	p.ch = ch
	return nil
}

// HeaderRequestID is the AMQP header that carries the request ID to the worker.
const HeaderRequestID = "x-request-id"

// newPublishing builds the message, adding the request ID header when ctx has one.
func newPublishing(ctx context.Context, body []byte) amqp.Publishing {
	msg := amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		ContentType:  "application/json",
		Timestamp:    time.Now(),
		Body:         body,
	}
	if id, ok := ctxkey.RequestID(ctx); ok {
		msg.Headers = amqp.Table{HeaderRequestID: id}
	}
	return msg
}

// requestIDOf returns the request ID header of d, or "" if absent or not a string.
func requestIDOf(d amqp.Delivery) string {
	id, _ := d.Headers[HeaderRequestID].(string)
	return id
}

// ridField is a zap field for the request ID, or zap.Skip() when empty.
func ridField(id string) zap.Field {
	if id == "" {
		return zap.Skip()
	}
	return zap.String("request_id", id)
}

// Publish serialises payload as JSON and publishes it to the shortlink exchange.
// If the channel has been closed it attempts to reopen once before returning an error.
func (p *Publisher) Publish(ctx context.Context, routingKey string, payload any) error {
	err := p.publish(ctx, routingKey, payload)
	result := "ok"
	if err != nil {
		result = "error"
	}
	metrics.Publishes.WithLabelValues(routingKey, result).Inc()
	return err
}

func (p *Publisher) publish(ctx context.Context, routingKey string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("publisher: marshal: %w", err)
	}

	msg := newPublishing(ctx, body)

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil || p.ch == nil {
		return errors.New("publisher: not connected")
	}
	if err := p.ch.PublishWithContext(ctx, Exchange, routingKey, false, false, msg); err != nil {
		p.log.Warn("publish failed, reopening channel", zap.Error(err))
		if reopenErr := p.openChannel(); reopenErr != nil {
			return fmt.Errorf("publisher: reopen channel: %w", reopenErr)
		}
		if err := p.ch.PublishWithContext(ctx, Exchange, routingKey, false, false, msg); err != nil {
			return fmt.Errorf("publisher: publish retry: %w", err)
		}
	}
	return nil
}

// Close stops reconnecting and closes the connection.
func (p *Publisher) Close() error {
	p.closeOnce.Do(func() { close(p.done) })
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		return nil
	}
	return p.conn.Close()
}
