package messaging

import (
	"context"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/service"
)

func TestNewPublishing_HeaderFromContext(t *testing.T) {
	ctx := ctxkey.SetRequestID(context.Background(), "rid-1")
	m := newPublishing(ctx, []byte("{}"))
	if m.Headers[HeaderRequestID] != "rid-1" {
		t.Errorf("headers = %v", m.Headers)
	}
	if m.DeliveryMode != amqp.Persistent || m.ContentType != "application/json" || string(m.Body) != "{}" {
		t.Errorf("base fields lost: %+v", m)
	}
}

func TestNewPublishing_NoHeaderWithoutID(t *testing.T) {
	if m := newPublishing(context.Background(), nil); m.Headers != nil {
		t.Errorf("want nil headers, got %v", m.Headers)
	}
}

func TestRequestIDOf(t *testing.T) {
	cases := []struct {
		h    amqp.Table
		want string
	}{
		{nil, ""},
		{amqp.Table{HeaderRequestID: "x"}, "x"},
		{amqp.Table{HeaderRequestID: 5}, ""},
	}
	for _, c := range cases {
		if got := requestIDOf(amqp.Delivery{Headers: c.h}); got != c.want {
			t.Errorf("%v: got %q want %q", c.h, got, c.want)
		}
	}
}

func TestNotificationConsumer_LogsRequestID(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	c := &NotificationConsumer{repo: &fakeNotifRepo{}, log: zap.New(core)}
	c.handle(context.Background(), amqp.Delivery{
		Acknowledger: &fakeAck{},
		RoutingKey:   service.RoutePasswordChanged,
		Body:         []byte(`{"dedupe_key":"p:1","user_id":7}`),
		Headers:      amqp.Table{HeaderRequestID: "rid-9"},
	})
	es := logs.FilterMessage("notification stored").All()
	if len(es) != 1 {
		t.Fatalf("want 1 stored line, got %d", len(es))
	}
	f := es[0].ContextMap()
	if f["request_id"] != "rid-9" || f["routing_key"] != service.RoutePasswordChanged {
		t.Errorf("fields = %v", f)
	}

	// rejected message: warn line gains request_id
	core2, logs2 := observer.New(zap.WarnLevel)
	c2 := &NotificationConsumer{repo: &fakeNotifRepo{}, log: zap.New(core2)}
	c2.handle(context.Background(), amqp.Delivery{
		Acknowledger: &fakeAck{}, RoutingKey: "bogus",
		Headers: amqp.Table{HeaderRequestID: "rid-9"},
	})
	if w := logs2.All(); len(w) != 1 || w[0].ContextMap()["request_id"] != "rid-9" {
		t.Errorf("reject log = %v", w)
	}
}

func TestClickFlush_FailureLogsRequestIDs(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	b := newClickBatch()
	ack := &fakeAck{}
	b.add(amqp.Delivery{Acknowledger: ack, Headers: amqp.Table{HeaderRequestID: "r1"}}, "a")
	b.add(amqp.Delivery{Acknowledger: ack}, "b")
	b.add(amqp.Delivery{Acknowledger: ack, Headers: amqp.Table{HeaderRequestID: "r2"}}, "c")
	b.flush(context.Background(), failingRepo{}, zap.New(core))

	es := logs.All()
	if len(es) != 1 {
		t.Fatalf("want 1 error line, got %d", len(es))
	}
	ids, _ := es[0].ContextMap()["request_ids"].([]any)
	if len(ids) != 2 || ids[0] != "r1" || ids[1] != "r2" {
		t.Errorf("request_ids = %v", es[0].ContextMap()["request_ids"])
	}
}

func TestDLQConsumer_LogsRequestID(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	c := &DLQConsumer{log: zap.New(core)}
	ack := &fakeAck{}
	c.handle(amqp.Delivery{Acknowledger: ack, RoutingKey: "k", Headers: amqp.Table{HeaderRequestID: "rid-3"}})
	es := logs.All()
	if len(es) != 1 || es[0].ContextMap()["request_id"] != "rid-3" || ack.acked != 1 {
		t.Errorf("logs=%v acked=%d", es, ack.acked)
	}
}
