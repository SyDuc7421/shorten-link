# Contributing

How people and AI coding agents work on this codebase. Read
[docs/architect.md](docs/architect.md) first for the big picture and
[docs/spec.md](docs/spec.md) for what the system must do.

---

## 1. Set up

**You need:** Go 1.26+ (go.mod pins toolchain 1.26.9; `GOTOOLCHAIN=auto` downloads it), Docker (for local services and integration tests),
`openssl`, and optionally `golang-migrate` and `golangci-lint`.

```bash
cp .env.example .env          # then edit passwords
make keys                     # RSA key pair for JWT → ./keys (git-ignored)
make docker-up                # MySQL, Redis, RabbitMQ
make migrate                  # apply deploy/migrations
make run                      # server on :8080, docs at http://localhost:8080/docs
set -a && . ./.env && set +a && go run ./cmd/worker   # click counter, separate terminal
```

## 2. Everyday commands

| Command | What it does |
|---|---|
| `make test` | All tests. Integration tests start real MySQL/Redis in Docker via testcontainers (~1 min) |
| `go test ./internal/service/` | Fast unit tests only for one package |
| `go test ./... -run TestName` | One test |
| `go vet ./...` | Static checks; must be clean |
| `make lint` | golangci-lint |
| `make build` | Binaries into `bin/` |

Docker must be running for `internal/repository` and `internal/middleware`
tests. If Docker is not available, run the other packages directly.

---

## 3. Where code goes

```
cmd/server, cmd/worker    wiring only: config → clients → repos → services → handlers
internal/handler          HTTP: decode JSON, call service, map errors to status codes
internal/service          business rules + the interfaces they need
internal/repository       MySQL / Redis implementations of those interfaces
internal/messaging        RabbitMQ
internal/middleware       cross-cutting HTTP concerns
pkg/idgen                 reusable, no project imports
deploy/migrations         SQL, numbered NNN_name.up.sql / .down.sql
```

Rules the code already follows. Keep them.

1. **Interfaces are defined by the consumer.** `service` declares
   `LinkRepository`; `repository` implements it. `handler` declares
   `LinkServicer`; `service` implements it.
2. **Errors are sentinels in `service`** (`ErrNotFound`, `ErrInvalidURL`,
   `ErrEmailTaken`, …). Repositories translate driver errors into them;
   handlers translate them into HTTP status codes. Wrap with `%w` so
   `errors.Is` works.
3. **Handlers never contain business rules.** If you write an `if` about the
   domain in a handler, it belongs in the service.
4. **No new packages for single-use code.** Prefer a function next to its only caller.
5. **Config comes from env vars** in `config/config.go`; add every new
   variable to `.env.example` with a comment.

### Example: adding an endpoint

1. Add the sentinel error(s) and interface method(s) in `internal/service`.
2. Write a failing service test with the in-file mocks, then implement.
3. If storage changes: add a migration, write a failing integration test in
   `internal/repository/integration_test.go`, then implement.
4. Add the handler method and a failing handler test (`httptest` + mock
   service), then implement and register the route in `Register`.
5. Update `api/openapi.yaml` and, if behaviour or limits change, `docs/spec.md`.

---

## 4. How we change code

### Test first

Every bug fix and behaviour change starts with a test that fails for the
right reason. Then write the smallest change that makes it pass, then run the
whole suite.

- Bug fix: reproduce the bug in a test first. The git history on
  `fix/review-issues` has examples.
- Code that is hard to test (inside `main`, a closure): extract a small
  function **without changing behaviour**, write the test, watch it fail, then fix.
- Prefer real dependencies (testcontainers) for repository code; mocks hide
  mapping bugs. The `expires_at` bug survived because only mocks covered it.

### Keep diffs small

- Touch only what the task needs. Don't reformat or refactor neighbouring code.
- Some older files are not `gofmt`-clean. Run `gofmt -w` on files you edit, not
  on the whole tree, so reviews stay readable.
- If you notice unrelated problems, write them down (issue, PR description,
  `docs/spec.md` status) instead of fixing them in the same change.

### Commits and branches

- Never commit to `master` directly. Branch: `fix/…`, `feat/…`, `docs/…`.
- [Conventional commits](https://www.conventionalcommits.org): `fix(scope): …`,
  `feat: …`, `test: …`, `docs: …`. The body says **why**, not only what.
- One logical change per commit; every commit builds and passes tests.

### Before opening a PR

```bash
go vet ./... && go test ./...
```

In the PR description, state what you verified and what you didn't (for
example "not tested against a real RabbitMQ").

---

## 5. Working with AI agents

The repo is set up for agents such as Claude Code.

| File / folder | Purpose |
|---|---|
| `CLAUDE.md` | Behaviour rules every agent loads: think first, simplest solution, surgical changes, verify against goals |
| `docs/superpowers/specs/` | Design documents written **before** big changes |
| `docs/superpowers/plans/` | Step-by-step implementation plans derived from a spec |
| `docs/architect.md`, `docs/spec.md` | Context an agent should read before touching unfamiliar code |

### Recommended flow for non-trivial work

1. **Design** → write or update a spec in `docs/superpowers/specs/` and get a human to agree.
2. **Plan** → break it into small tasks in `docs/superpowers/plans/`, each with
   files to touch and how to verify.
3. **Execute** → one task at a time, test first, one commit per task.
4. **Verify** → full `go test ./...` and `go vet ./...`, then review the diff.

### Rules for agents

- Read the relevant code before changing it; don't infer behaviour from names.
- Ask when the request is ambiguous instead of picking silently.
- Do not commit secrets, `.env`, `keys/`, or build output (the root `worker`
  binary is a local build artifact; use `make build` → `bin/`).
- Report test results truthfully, including failures you did not cause.
- Never push, merge, or deploy without a human asking for it.

### Rules for humans reviewing agent work

- Check that each fix has a test that would fail without it.
- Check that the diff only touches what the task asked for.
- Ask the agent for the exact commands it ran and their output when in doubt.
