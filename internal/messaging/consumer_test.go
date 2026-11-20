package messaging

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/metrics"
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

type okRepo struct{ service.LinkRepository }

func (okRepo) BatchUpdateClicks(context.Context, map[string]int64) error { return nil }

func histCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestClickFlush_SuccessCountsAckAndBatch(t *testing.T) {
	ack := &fakeAck{}
	b := newClickBatch()
	b.add(amqp.Delivery{Acknowledger: ack}, "a")
	b.add(amqp.Delivery{Acknowledger: ack}, "b")

	acks := metrics.WorkerMessages.WithLabelValues(QueueClicks, "ack")
	flushes := metrics.ClickFlushes.WithLabelValues("ok")
	a0, f0 := testutil.ToFloat64(acks), testutil.ToFloat64(flushes)
	h0 := histCount(t, metrics.ClickBatchSize)

	b.flush(context.Background(), okRepo{}, zap.NewNop())

	if d := testutil.ToFloat64(acks) - a0; d != 2 {
		t.Errorf("ack delta = %v, want 2", d)
	}
	if d := testutil.ToFloat64(flushes) - f0; d != 1 {
		t.Errorf("flush ok delta = %v, want 1", d)
	}
	if d := histCount(t, metrics.ClickBatchSize) - h0; d != 1 {
		t.Errorf("batch size observations delta = %d, want 1", d)
	}
	// Empty flush records nothing.
	b.flush(context.Background(), okRepo{}, zap.NewNop())
	if d := testutil.ToFloat64(flushes) - f0; d != 1 {
		t.Errorf("empty flush counted: delta = %v", d)
	}
}

func TestClickFlush_FailureCountsNackThenDLQ(t *testing.T) {
	ack := &fakeAck{}
	b := newClickBatch()
	b.add(amqp.Delivery{Acknowledger: ack, Redelivered: false}, "a")
	b.add(amqp.Delivery{Acknowledger: ack, Redelivered: true}, "b")

	nack := metrics.WorkerMessages.WithLabelValues(QueueClicks, "nack")
	dlq := metrics.WorkerMessages.WithLabelValues(QueueClicks, "dlq")
	fail := metrics.ClickFlushes.WithLabelValues("error")
	n0, d0, f0 := testutil.ToFloat64(nack), testutil.ToFloat64(dlq), testutil.ToFloat64(fail)

	b.flush(context.Background(), failingRepo{}, zap.NewNop())

	if d := testutil.ToFloat64(nack) - n0; d != 1 {
		t.Errorf("nack delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(dlq) - d0; d != 1 {
		t.Errorf("dlq delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(fail) - f0; d != 1 {
		t.Errorf("flush error delta = %v, want 1", d)
	}
}
