# `cmd/worker`: clicks, notifications and expiry scan

**What it does.** The second program built from this codebase. It reads click
events from RabbitMQ and adds them to each link's `clicks` column in MySQL. It
also reads notification events and stores them in the `notifications` table.
It also **publishes**: a timer scans for links about to expire and publishes
`link.expiring` events. It has no HTTP server.

**Where it sits.** It uses `config`, `internal/messaging`,
`internal/repository` and `internal/service` (for the expiry scanner).
Nothing imports it. The server and the worker share no memory; they only meet
through RabbitMQ and MySQL.

## What happens when it runs

1. Load config and create the logger (exit 1 on failure).
2. Connect to MySQL and RabbitMQ. **Both are required**: unlike the server, the
   worker has nothing useful to do without them, so it exits and lets Docker
   restart it.
3. `messaging.Setup` declares the exchanges and queues.
4. Open one channel per consumer and start four consumers, each in its own
   goroutine: `ClickConsumer`, a `DLQConsumer` on `clicks.dlq`, the
   `NotificationConsumer` (writes notification rows to MySQL) and a
   `DLQConsumer` on `notifications.dlq`. The details of batching and
   dead-lettering are in
   [messaging.md](messaging.md#the-consumers-consumergo).
5. Create a publisher (`messaging.NewPublisher`, which reconnects on its own)
   and start the `ExpiryScanner` in a fifth goroutine. Every
   `EXPIRY_SCAN_INTERVAL` (default 5 minutes) it publishes `link.expiring` for
   links that expire within 24 hours; see
   [service.md](service.md#expiry-scanner-expiry_scannergo). A broker outage
   only makes publishes fail and be retried on the next scan.
6. Wait in `waitForShutdown`.

## Shutdown and failure

`waitForShutdown` returns when either:

- **a signal arrives** (`SIGINT`, `SIGTERM`): it cancels the context, waits for
  all four consumers and the scanner to
  return (the click consumer flushes its last batch first),
  and the process exits 0; or
- **a consumer stops on its own**, usually because the RabbitMQ connection
  dropped: it cancels the others, waits for them, and the process exits
  **1**. Exiting non-zero tells Docker (`restart: unless-stopped`) that
  something went wrong, and the restart reconnects from scratch.

The consumers do not reconnect by themselves; the worker's publisher does, the
same way the server's publisher does. Restarting the process is simpler and loses nothing: unacknowledged messages
stay in the queue and are delivered again.

## Running it

```bash
# locally, in its own terminal, after make docker-up and make migrate
set -a && . ./.env && set +a && go run ./cmd/worker
```

`config.Load` requires the JWT key paths even though the worker never uses
them, so the same `.env` works for both programs.

Scaling: several workers can consume the same queue; RabbitMQ spreads messages
between them. `BatchUpdateClicks` uses relative updates (`clicks = clicks + ?`),
so parallel workers do not overwrite each other, and it updates codes in sorted
order, so two workers lock rows in the same order and cannot deadlock.

## Before you change it

- Tests: `cmd/worker/main_test.go` covers both shutdown paths of
  `waitForShutdown` (signal, and a consumer exiting early).
- If you add a consumer, start it in its own goroutine, count it in the `n`
  passed to `waitForShutdown`, and give it its own channel. AMQP channels are
  not meant to be shared between goroutines.
