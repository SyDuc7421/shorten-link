package messaging

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	Exchange    = "shortlink"       // topic exchange
	dlxName     = "shortlink.dlx"  // dead-letter exchange (direct)
	QueueClicks = "clicks"
	QueueDLQ    = "clicks.dlq"
)

// Setup idempotently declares all AMQP resources required by the service.
// Safe to call on every startup.
func Setup(conn *amqp.Connection) error {
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("messaging setup: open channel: %w", err)
	}
	defer ch.Close()

	// main exchange
	if err := ch.ExchangeDeclare(Exchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("messaging setup: declare exchange: %w", err)
	}

	// dead-letter exchange (direct)
	if err := ch.ExchangeDeclare(dlxName, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("messaging setup: declare dlx: %w", err)
	}

	// DLQ — receives rejected messages from the clicks queue
	if _, err := ch.QueueDeclare(QueueDLQ, true, false, false, false, nil); err != nil {
		return fmt.Errorf("messaging setup: declare dlq: %w", err)
	}
	if err := ch.QueueBind(QueueDLQ, "clicks.dead", dlxName, false, nil); err != nil {
		return fmt.Errorf("messaging setup: bind dlq: %w", err)
	}

	// clicks queue with DLX routing
	args := amqp.Table{
		"x-dead-letter-exchange":     dlxName,
		"x-dead-letter-routing-key":  "clicks.dead",
	}
	if _, err := ch.QueueDeclare(QueueClicks, true, false, false, false, args); err != nil {
		return fmt.Errorf("messaging setup: declare clicks queue: %w", err)
	}
	if err := ch.QueueBind(QueueClicks, "link.clicked", Exchange, false, nil); err != nil {
		return fmt.Errorf("messaging setup: bind clicks queue: %w", err)
	}

	return nil
}
