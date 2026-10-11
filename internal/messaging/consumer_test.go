package messaging

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/service"
)

// fakeAck records how each delivery was settled.
type fakeAck struct {
	acked   int
	requeue []bool // one entry per Nack
}

func (f *fakeAck) Ack(uint64, bool) error { f.acked++; return nil }
func (f *fakeAck) Nack(_ uint64, _ bool, requeue bool) error {
	f.requeue = append(f.requeue, requeue)
	return nil
}
func (f *fakeAck) Reject(uint64, bool) error { return nil }

type failingRepo struct{ service.LinkRepository }

func (failingRepo) BatchUpdateClicks(context.Context, map[string]int64) error {
	return errors.New("db down")
}

func TestClickBatch_FailedRedeliveryGoesToDLQ(t *testing.T) {
	ack := &fakeAck{}
	b := newClickBatch()
	b.add(amqp.Delivery{Acknowledger: ack, Redelivered: false}, "a")
	b.add(amqp.Delivery{Acknowledger: ack, Redelivered: true}, "b")

	b.flush(context.Background(), failingRepo{}, zap.NewNop())

	if len(ack.requeue) != 2 {
		t.Fatalf("want 2 nacks, got %d", len(ack.requeue))
	}
	if !ack.requeue[0] {
		t.Error("first delivery: want requeue=true for one retry")
	}
	if ack.requeue[1] {
		t.Error("redelivered message: want requeue=false so it dead-letters")
	}
}
