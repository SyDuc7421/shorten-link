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

	done      chan struct{}
	closeOnce sync.Once
}

// NewPublisher connects using dial. It does not fail if the broker is
// unreachable; it keeps retrying in the background instead.
func NewPublisher(dial func() (*amqp.Connection, error), log *zap.Logger) *Publisher {
	p := &Publisher{dial: dial, log: log, done: make(chan struct{})}
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
		case <-time.After(reconnectDelay):
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

// Publish serialises payload as JSON and publishes it to the shortlink exchange.
// If the channel has been closed it attempts to reopen once before returning an error.
func (p *Publisher) Publish(ctx context.Context, routingKey string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("publisher: marshal: %w", err)
	}

	msg := amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		ContentType:  "application/json",
		Timestamp:    time.Now(),
		Body:         body,
	}

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
