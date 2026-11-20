package messaging

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	amqp "github.com/rabbitmq/amqp091-go"
	tcrabbit "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/metrics"
)

// setupRabbit starts a broker and returns it with a dial func that re-reads the
// mapped port on every call (it changes when the container restarts).
func setupRabbit(t *testing.T) (*tcrabbit.RabbitMQContainer, func() (*amqp.Connection, error)) {
	t.Helper()
	ctx := context.Background()
	c, err := tcrabbit.Run(ctx, "rabbitmq:3.13-alpine")
	if err != nil {
		t.Fatalf("start rabbitmq container: %v", err)
	}
	t.Cleanup(func() { c.Terminate(ctx) }) //nolint:errcheck

	dial := func() (*amqp.Connection, error) {
		url, err := c.AmqpURL(ctx)
		if err != nil {
			return nil, err
		}
		return amqp.Dial(url)
	}
	return c, dial
}

// setReconnectDelay overrides the package default for one test and restores it
// afterwards. Publishers copy the value at construction, so their loops never
// read the global.
func setReconnectDelay(t *testing.T, d time.Duration) {
	t.Helper()
	old := reconnectDelay
	reconnectDelay = d
	t.Cleanup(func() { reconnectDelay = old })
}

// eventually retries f until it returns nil or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still failing after %s: %v", what, timeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireDelivered checks that a click event with the given code is in the clicks queue.
func requireDelivered(t *testing.T, dial func() (*amqp.Connection, error), code string) {
	t.Helper()
	conn, err := dial()
	if err != nil {
		t.Fatalf("dial for verification: %v", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	for {
		msg, ok, err := ch.Get(QueueClicks, true)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !ok {
			t.Fatalf("click %q never reached queue %s", code, QueueClicks)
		}
		if strings.Contains(string(msg.Body), code) {
			return
		}
	}
}

func publishClick(p *Publisher, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.Publish(ctx, "link.clicked", map[string]any{"code": code})
}

func TestPublisher_RecoversFromBrokerRestart(t *testing.T) {
	setReconnectDelay(t, 200*time.Millisecond)
	c, dial := setupRabbit(t)
	ctx := context.Background()

	p := NewPublisher(dial, zap.NewNop())
	defer p.Close()
	eventually(t, 10*time.Second, "initial publish", func() error { return publishClick(p, "before") })

	if err := c.Stop(ctx, nil); err != nil {
		t.Fatalf("stop broker: %v", err)
	}
	eventually(t, 10*time.Second, "detect broker down", func() error {
		if !p.IsClosed() {
			return errors.New("publisher still reports connected")
		}
		return nil
	})
	start := time.Now()
	if err := publishClick(p, "while-down"); err == nil {
		t.Error("publish succeeded while broker was down")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("publish while down took %s; must fail fast", d)
	}

	if err := c.Start(ctx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	eventually(t, 60*time.Second, "publish after restart", func() error { return publishClick(p, "after-restart") })
	requireDelivered(t, dial, "after-restart")
}

func TestPublisher_StartsWhileBrokerUnavailable(t *testing.T) {
	setReconnectDelay(t, 200*time.Millisecond)
	_, dial := setupRabbit(t)

	var attempts atomic.Int32
	flakyDial := func() (*amqp.Connection, error) {
		if attempts.Add(1) <= 3 {
			return nil, errors.New("connection refused")
		}
		return dial()
	}

	p := NewPublisher(flakyDial, zap.NewNop())
	defer p.Close()
	if !p.IsClosed() {
		t.Error("want IsClosed while broker unreachable")
	}
	if err := publishClick(p, "too-early"); err == nil {
		t.Error("publish succeeded before any connection existed")
	}

	eventually(t, 10*time.Second, "publish after broker becomes reachable", func() error { return publishClick(p, "late-start") })
	requireDelivered(t, dial, "late-start")
}

func TestPublish_NotConnectedCountsError(t *testing.T) {
	setReconnectDelay(t, time.Hour)
	p := NewPublisher(func() (*amqp.Connection, error) { return nil, errors.New("refused") }, zap.NewNop())
	defer p.Close()

	c := metrics.Publishes.WithLabelValues("link.clicked", "error")
	before := testutil.ToFloat64(c)
	start := time.Now()
	if err := publishClick(p, "x"); err == nil {
		t.Fatal("want error when not connected")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("publish must fail fast")
	}
	if d := testutil.ToFloat64(c) - before; d != 1 {
		t.Errorf("publish error delta = %v, want 1", d)
	}
}

func TestPublish_SuccessCountsOK(t *testing.T) {
	setReconnectDelay(t, 200*time.Millisecond)
	_, dial := setupRabbit(t)
	p := NewPublisher(dial, zap.NewNop())
	defer p.Close()

	ok := metrics.Publishes.WithLabelValues("link.clicked", "ok")
	before := testutil.ToFloat64(ok)
	eventually(t, 10*time.Second, "publish", func() error { return publishClick(p, "metric") })
	if d := testutil.ToFloat64(ok) - before; d != 1 {
		t.Errorf("publish ok delta = %v, want 1", d)
	}
}
