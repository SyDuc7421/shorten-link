package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

// Publisher publishes events to RabbitMQ. It satisfies service.Publisher.
// It reconnects automatically when the underlying channel is closed.
type Publisher struct {
	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
	log  *zap.Logger
	url  string // kept for reconnect
}

// NewPublisher opens a channel on conn and returns a ready Publisher.
func NewPublisher(conn *amqp.Connection, log *zap.Logger) (*Publisher, error) {
	p := &Publisher{conn: conn, log: log}
	if err := p.openChannel(); err != nil {
		return nil, err
	}
	return p, nil
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

// Close closes the underlying channel.
func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ch.Close()
}
