package messaging

import (
	"context"
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/service"
)

type fakeNotifRepo struct {
	service.NotificationRepository
	err   error
	got   []*service.Notification
	dedup []string
}

func (f *fakeNotifRepo) Insert(ctx context.Context, n *service.Notification, dedupeKey string) error {
	if f.err != nil {
		return f.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.got = append(f.got, n)
	f.dedup = append(f.dedup, dedupeKey)
	return nil
}

func newTestNotifConsumer(repo service.NotificationRepository) *NotificationConsumer {
	return &NotificationConsumer{repo: repo, log: zap.NewNop()}
}

func TestNotificationConsumer_InsertsAndAcks(t *testing.T) {
	cases := []struct {
		route, typ, body, dedupe string
		linkID                   int64
		code                     string
	}{
		{service.RouteLinkDisabled, service.NotificationLinkDisabled,
			`{"dedupe_key":"disabled:u1","user_id":7,"link_id":11,"short_code":"abc"}`, "disabled:u1", 11, "abc"},
		{service.RouteLinkEnabled, service.NotificationLinkEnabled,
			`{"dedupe_key":"enabled:u2","user_id":7,"link_id":12,"short_code":"def"}`, "enabled:u2", 12, "def"},
		{service.RouteLinkExpiring, service.NotificationLinkExpiring,
			`{"dedupe_key":"expiring:13","user_id":7,"link_id":13,"short_code":"ghi","expires_at":"2030-01-02T03:04:05Z"}`, "expiring:13", 13, "ghi"},
		{service.RoutePasswordChanged, service.NotificationPasswordChanged,
			`{"dedupe_key":"password:u3","user_id":7}`, "password:u3", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			repo := &fakeNotifRepo{}
			ack := &fakeAck{}
			newTestNotifConsumer(repo).handle(context.Background(),
				amqp.Delivery{Acknowledger: ack, Body: []byte(tc.body), RoutingKey: tc.route})

			if ack.acked != 1 || len(ack.requeue) != 0 {
				t.Fatalf("want single ack, got acked=%d nacks=%v", ack.acked, ack.requeue)
			}
			if len(repo.got) != 1 {
				t.Fatalf("want 1 insert, got %d", len(repo.got))
			}
			n := repo.got[0]
			if n.Type != tc.typ || n.UserID != 7 || repo.dedup[0] != tc.dedupe {
				t.Errorf("bad notification: %+v dedupe=%s", n, repo.dedup[0])
			}
			if tc.linkID == 0 {
				if n.LinkID != nil || n.ShortCode != nil {
					t.Errorf("want nil link fields, got %v %v", n.LinkID, n.ShortCode)
				}
			} else if n.LinkID == nil || *n.LinkID != tc.linkID || n.ShortCode == nil || *n.ShortCode != tc.code {
				t.Errorf("bad link fields: %v %v", n.LinkID, n.ShortCode)
			}
			if tc.route == service.RouteLinkExpiring && n.ExpiresAt == nil {
				t.Error("want ExpiresAt set for link.expiring")
			}
		})
	}
}

func TestNotificationConsumer_RejectsBadMessages(t *testing.T) {
	cases := map[string]struct{ route, body string }{
		"bad json":        {service.RouteLinkDisabled, `{not json`},
		"unknown route":   {"link.unknown", `{"dedupe_key":"x","user_id":1,"link_id":1,"short_code":"a"}`},
		"missing user_id": {service.RouteLinkDisabled, `{"dedupe_key":"x","link_id":1,"short_code":"a"}`},
		"missing link_id": {service.RouteLinkDisabled, `{"dedupe_key":"x","user_id":1,"short_code":"a"}`},
		"missing dedupe":  {service.RouteLinkDisabled, `{"user_id":1,"link_id":1,"short_code":"a"}`},
		"missing code":    {service.RouteLinkEnabled, `{"dedupe_key":"x","user_id":1,"link_id":1}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &fakeNotifRepo{}
			ack := &fakeAck{}
			newTestNotifConsumer(repo).handle(context.Background(),
				amqp.Delivery{Acknowledger: ack, Body: []byte(tc.body), RoutingKey: tc.route})
			if ack.acked != 0 || len(ack.requeue) != 1 || ack.requeue[0] {
				t.Fatalf("want one Nack requeue=false, got acked=%d nacks=%v", ack.acked, ack.requeue)
			}
			if len(repo.got) != 0 {
				t.Error("want no insert")
			}
		})
	}
}

func TestNotificationConsumer_RetriesOnceThenDLQ(t *testing.T) {
	repo := &fakeNotifRepo{err: errors.New("db down")}
	c := newTestNotifConsumer(repo)
	body := []byte(`{"dedupe_key":"password:u","user_id":1}`)

	first, second := &fakeAck{}, &fakeAck{}
	c.handle(context.Background(), amqp.Delivery{Acknowledger: first, Body: body, RoutingKey: service.RoutePasswordChanged})
	c.handle(context.Background(), amqp.Delivery{Acknowledger: second, Body: body, RoutingKey: service.RoutePasswordChanged, Redelivered: true})

	if len(first.requeue) != 1 || !first.requeue[0] || first.acked != 0 {
		t.Errorf("first: want one Nack requeue=true, got %v acked=%d", first.requeue, first.acked)
	}
	if len(second.requeue) != 1 || second.requeue[0] || second.acked != 0 {
		t.Errorf("redelivered: want one Nack requeue=false, got %v acked=%d", second.requeue, second.acked)
	}
}

func TestNotificationConsumer_DuplicateIsAcked(t *testing.T) {
	repo := &fakeNotifRepo{} // Insert returns nil, as the ON DUPLICATE KEY no-op does for a duplicate
	ack := &fakeAck{}
	newTestNotifConsumer(repo).handle(context.Background(), amqp.Delivery{
		Acknowledger: ack, RoutingKey: service.RoutePasswordChanged,
		Body: []byte(`{"dedupe_key":"password:dup","user_id":1}`),
	})
	if ack.acked != 1 || len(ack.requeue) != 0 {
		t.Fatalf("want exactly one ack, got acked=%d nacks=%v", ack.acked, ack.requeue)
	}
}

func TestNotificationConsumer_OccurredAtBecomesCreatedAt(t *testing.T) {
	repo := &fakeNotifRepo{}
	ack := &fakeAck{}
	body := `{"dedupe_key":"password:u3","user_id":7,"occurred_at":"2030-01-02T03:04:05Z"}`
	newTestNotifConsumer(repo).handle(context.Background(),
		amqp.Delivery{Acknowledger: ack, Body: []byte(body), RoutingKey: service.RoutePasswordChanged})
	want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if len(repo.got) != 1 || !repo.got[0].CreatedAt.Equal(want) {
		t.Fatalf("got %+v, want CreatedAt %v", repo.got, want)
	}
}

func TestNotificationConsumer_InFlightDeliveryFinishesAfterCancel(t *testing.T) {
	repo := &fakeNotifRepo{}
	ack := &fakeAck{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	newTestNotifConsumer(repo).handle(ctx,
		amqp.Delivery{Acknowledger: ack, Redelivered: true, Body: []byte(`{"dedupe_key":"password:u3","user_id":7}`), RoutingKey: service.RoutePasswordChanged})
	if ack.acked != 1 || len(ack.requeue) != 0 || len(repo.got) != 1 {
		t.Fatalf("want ack and 1 insert, got acked=%d nacks=%v inserts=%d", ack.acked, ack.requeue, len(repo.got))
	}
}
