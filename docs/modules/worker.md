# `cmd/worker`: the click counter

**What it does.** The second program built from this codebase. It reads click
events from RabbitMQ and adds them to each link's `clicks` column in MySQL.
It has no HTTP server.

**Where it sits.** It uses `config`, `internal/messaging` and
`internal/repository`. Nothing imports it. The server and the worker share no
memory; they only meet through RabbitMQ and MySQL.

## What happens when it runs

1. Load config and create the logger (exit 1 on failure).
2. Connect to MySQL and RabbitMQ. **Both are required**: unlike the server, the
   worker has nothing useful to do without them, so it exits and lets Docker
   restart it.
3. `messaging.Setup` declares the exchanges and queues.
4. Open one channel per consumer and start `ClickConsumer` and `DLQConsumer`,
   each in its own goroutine. The details of batching and dead-lettering are in
   [messaging.md](messaging.md#the-consumers-consumergo).
5. Wait in `waitForShutdown`.

## Shutdown and failure

`waitForShutdown` returns when either:

- **a signal arrives** (`SIGINT`, `SIGTERM`): it cancels the context, waits for
  both consumers to return (the click consumer flushes its last batch first),
  and the process exits 0; or
- **a consumer stops on its own**, usually because the RabbitMQ connection
  dropped: it cancels the other consumer, waits for it, and the process exits
  **1**. Exiting non-zero tells Docker (`restart: unless-stopped`) that
  something went wrong, and the restart reconnects from scratch.

The worker does not reconnect by itself the way the server's publisher does.
Restarting the process is simpler and loses nothing: unacknowledged messages
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
so parallel workers do not overwrite each other.

## Before you change it

- Tests: `cmd/worker/main_test.go` covers both shutdown paths of
  `waitForShutdown` (signal, and a consumer exiting early).
- If you add a consumer, start it in its own goroutine, count it in the `n`
  passed to `waitForShutdown`, and give it its own channel. AMQP channels are
  not meant to be shared between goroutines.
