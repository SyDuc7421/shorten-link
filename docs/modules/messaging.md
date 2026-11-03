# `internal/messaging`: RabbitMQ

**What it does.** Moves "someone clicked a link" from the server to the worker
through RabbitMQ, so a redirect never waits on a database write. It has three
parts: the topology (`setup.go`), the publisher used by the server
(`publisher.go`), and the consumers used by the worker (`consumer.go`, `notification_consumer.go`).

**Where it sits.** `cmd/server` creates a `Publisher` and hands it to
`service.LinkService`, `service.AdminService` and `service.AuthService`, which
see it only as a `service.Publisher`. `cmd/worker` creates the consumers and
gives them `repository.linkMySQL` to write counts and
`repository.NewNotificationMySQL` to store notifications. It also creates one
`Publisher` for the expiry scanner.

## The topology (`setup.go`)

```
server ──publish "link.clicked"──▶ exchange "shortlink" (topic)
                                        │ binding "link.clicked"
                                        ▼
                                  queue "clicks" ──consumed by──▶ ClickConsumer (worker)
                                        │ rejected twice
                                        ▼
                         exchange "shortlink.dlx" (direct)
                                        │ binding "clicks.dead"
                                        ▼
                                  queue "clicks.dlq" ──consumed by──▶ DLQConsumer (worker)

server ──publish notification event──▶ exchange "shortlink" (topic)
          (link.disabled, link.enabled,      │ one binding per routing key
           link.expiring,                    ▼
           user.password_changed)     queue "notifications" ──consumed by──▶ NotificationConsumer (worker)
                                             │ rejected twice, or malformed
                                             ▼
                                  exchange "shortlink.dlx" (direct)
                                             │ binding "notifications.dead"
                                             ▼
                                  queue "notifications.dlq" ──consumed by──▶ DLQConsumer (worker)
```

`Setup(conn)` declares all of this and is safe to call repeatedly; declaring
something that already exists with the same settings does nothing. Both
programs call it on every (re)connect, so whichever starts first creates the
queues. In particular, the `clicks` queue exists even if the worker has never
run, and clicks wait there instead of being dropped.

Exchanges and queues are **durable** and messages are published as
**persistent**, so they survive a RabbitMQ restart (as long as its data volume
survives too).

The `notifications` queue is bound to `shortlink` once per routing key:
`link.disabled`, `link.enabled`, `link.expiring` and `user.password_changed`.
Its dead-letter settings mirror `clicks`, with routing key
`notifications.dead`.

`link.created` is also published, but nothing is bound to it yet. Messages
with no matching queue are discarded by RabbitMQ.

## The publisher (`publisher.go`)

`Publish(ctx, routingKey, payload)` JSON-encodes the payload and publishes it
to the `shortlink` exchange.

**Built to survive RabbitMQ going away.** `NewPublisher` does not fail when the
broker is unreachable. A background goroutine (`reconnectLoop`) watches the
connection and, when it drops, re-dials every 2 seconds and re-runs `Setup`.
While disconnected, `Publish` fails immediately with `publisher: not connected`
instead of blocking. The server therefore starts, and keeps serving redirects,
without RabbitMQ.

If publishing on a channel fails (channels can close on their own after an
error), `Publish` reopens the channel once and retries.

`IsClosed()` reports whether there is a live connection. `/healthz` uses it.

**What is not guaranteed.** Clicks published while disconnected are lost: the
service ignores publish errors on purpose. Publisher confirms are not enabled
either, so a message in flight when the broker crashes can be lost. Click
counts are "best effort" by design (FR-S1 in [spec.md](../spec.md)).

## The consumers (`consumer.go`)

### `ClickConsumer`

Reads from `clicks` and adds the counts to MySQL **in batches**, because one
`UPDATE` per click would not keep up with a popular link.

1. Each message is decoded as `{"code": "..."}`. A malformed one is rejected
   without requeue, which sends it to the DLQ.
2. Valid messages accumulate in a `clickBatch`: a count per code, plus the
   deliveries that have not been acknowledged yet.
3. The batch is flushed when it reaches **500 messages** or **2 seconds** have
   passed, whichever comes first. Flushing calls
   `repo.BatchUpdateClicks(counts)`, which applies all counts in one transaction.
4. **On success**, every message in the batch is acknowledged.
   **On failure**, every message is rejected: messages seen for the first time
   are requeued, messages already redelivered once go to the DLQ. A database
   that stays down cannot make the same batch loop forever.

`Qos(500)` tells RabbitMQ to hand the consumer at most 500 unacknowledged
messages at a time, which matches the batch size.

When the context is cancelled (shutdown) or the channel closes, the consumer
flushes what it has before returning, so nothing already received is lost.

### `DLQConsumer`

`NewDLQConsumer(ch, queue, log)` consumes the queue it is given. The worker
runs two: one on `clicks.dlq` and one on `notifications.dlq` (consumer tag is
`<queue>-consumer`). Each logs every message that lands there at error level,
with its body and routing key, then acknowledges it. Dead-lettered clicks are
**not** counted and dead-lettered notifications are **not** stored. The log
line is the only trace, so alert on `dead-letter message received`.

## The notification consumer (`notification_consumer.go`)

`NotificationConsumer` reads `notifications` and stores one row per event with
`NotificationRepository.Insert`. It handles one message at a time (`Qos(50)`
only limits how many are in flight). `handle` settles each delivery:

1. The routing key must be one of the four known keys
   (`service.NotificationTypeForRoute` maps it to the notification type).
   Unknown key, bad JSON, `user_id == 0` or an empty `dedupe_key`: reject
   without requeue, so the message goes to `notifications.dlq`.
2. For `link.disabled`, `link.enabled` and `link.expiring`, `link_id == 0` or
   an empty `short_code` is rejected the same way. `user.password_changed`
   carries no link, so its `LinkID` and `ShortCode` stay nil.
3. `Insert` succeeds: ack. A duplicate `dedupe_key` is not an error (the
   insert is `INSERT ... ON DUPLICATE KEY UPDATE dedupe_key = dedupe_key`, a
   no-op), so a redelivered or republished event is acked and stores nothing
   new. The row's `created_at` is the event's `occurred_at`, so a backlog does
   not change the time shown. `Insert` runs on a context that ignores
   shutdown, so a delivery already taken finishes instead of failing.
4. `Insert` fails: same retry-once rule as clicks. First failure is requeued,
   a redelivered message is dead-lettered.

## Before you change it

- **Dead-lettered notification events are lost.** `DLQConsumer` on
  `notifications.dlq` only logs them; nothing replays them.
- **Changing a queue's arguments** (for example adding a TTL or max length)
  makes `QueueDeclare` fail against the existing queue with
  `PRECONDITION_FAILED`. You have to delete the queue, or use a new name, as
  part of the deploy.
- **New event type:** publish it with a new routing key, and bind a new queue
  in `Setup` if something should consume it.
- `clicks` has no length limit. If the worker stays down while traffic is
  high, the queue keeps growing in RabbitMQ's memory and disk.
- Tests: the publisher tests start a real RabbitMQ in Docker and check that
  the publisher starts without a broker and recovers after the container is
  stopped and started again. `consumer_test.go` checks the
  requeue-then-dead-letter rule with a fake acknowledger, without Docker, and
  `notification_consumer_test.go` does the same for `NotificationConsumer.handle`
  with a fake repository. The
  batching loop in `ClickConsumer.Start` has no direct test yet.
