# Module guide

One page per module, written for a developer who is new to the codebase.
Each page answers the same questions in the same order:

1. **What it does**, in a sentence or two.
2. **Where it sits**: who calls it and what it calls.
3. **How it works**: the main flow, with the types and functions you will meet.
4. **Why it is built this way**: the decisions that are not obvious from the code.
5. **Before you change it**: traps, invariants and the tests that cover it.

Start with [architect.md](../architect.md) for the big picture (the two programs,
the backing services, what happens on a redirect). Come back here when you are
about to work inside a specific package.

## Reading order

If you read everything, this order follows a request from the outside in:

| # | Module | Page | One-line summary |
|---|---|---|---|
| 1 | `cmd/server` | [server.md](server.md) | Wires the HTTP server together: config, clients, middleware, rate limits, shutdown |
| 2 | `internal/middleware` | [middleware.md](middleware.md) | Code that runs around every request: auth, CORS, rate limits, headers, body size |
| 3 | `internal/handler` | [handler.md](handler.md) | Turns HTTP requests into service calls and service errors into status codes |
| 4 | `internal/service` | [service.md](service.md) | The business rules for links and for accounts/tokens |
| 5 | `internal/repository` | [repository.md](repository.md) | MySQL and Redis code behind the service interfaces |
| 6 | `deploy/migrations` | [database.md](database.md) | The schema, and how to change it |
| 7 | `internal/messaging` | [messaging.md](messaging.md) | RabbitMQ: publishing click events and consuming them in batches |
| 8 | `cmd/worker` | [worker.md](worker.md) | The second program, which counts clicks |
| 9 | `config` | [config.md](config.md) | Environment variables, connections to MySQL/Redis/RabbitMQ, RSA keys |
| 10 | `pkg/idgen` | [idgen.md](idgen.md) | Snowflake IDs and the Base62 short codes made from them |
| 11 | `internal/ctxkey`, `internal/logger`, `api` | [support.md](support.md) | Small helper packages |

## The one rule that explains the layout

Dependencies point inward, and **interfaces live with the code that uses them**:

```
handler ──uses──▶ LinkServicer (declared in handler)  ◀──implemented by── service
service ──uses──▶ LinkRepository (declared in service) ◀──implemented by── repository
```

So `service` never imports `repository`, and `handler` never imports
`repository` either. Only `cmd/server` and `cmd/worker` know about every
package, because they are where concrete types get plugged into interfaces.
This is why most tests can swap in a small hand-written mock.

## Keeping these pages true

These pages describe the code as it is. When you add a module or change how
one behaves, update its page in the same commit (see
[CONTRIBUTING.md](../../CONTRIBUTING.md#keep-docs-in-sync)). A page that lies is
worse than no page.
