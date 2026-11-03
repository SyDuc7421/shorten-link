# Notifications (backend) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Link owners get in-app notifications when an admin disables or enables their link and about 24 hours before it expires. Users get an in-app notification and an email when their password is changed.

**Architecture:** The server publishes events on the existing `shortlink` topic exchange. The worker consumes them from a new `notifications` queue and writes rows to a new MySQL table. The worker also runs an expiry scanner that publishes `link.expiring`. The server serves the rows over `/api/notifications`. The password-changed email is sent directly by the server, not through RabbitMQ.

**Tech Stack:** Go 1.x, net/http ServeMux, sqlx + MySQL, amqp091-go (RabbitMQ), zap, envconfig, testcontainers (repository integration tests).

**Spec:** `docs/superpowers/specs/2026-11-03-notifications-design.md`

## Global Constraints

- Routing keys, exactly: `link.disabled`, `link.enabled`, `link.expiring`, `user.password_changed`.
- Notification `type` values, exactly: `link_disabled`, `link_enabled`, `link_expiring`, `password_changed`.
- Dedupe keys: `disabled:<uuid>`, `enabled:<uuid>`, `password:<uuid>` (one per action) and `expiring:<link_id>` (one per link, ever). Unique in the DB. *(Plan change from the spec: the spec says a snowflake id. A uuid (`github.com/google/uuid`, already a dependency) serves the same purpose and means `AdminService` does not need an id generator.)*
- Expiry warning window: 24 hours, a constant. Links whose `expires_at - created_at` is 24 hours or less are never warned.
- Publishing failures never fail or slow down `DisableLink`, `EnableLink` or `ResetPassword`. Publish in the background with the existing `publishTimeout` (2s) and log a warning on error.
- Admin events are published only when the repository reports `changed == true`.
- The admin's `reason` is never put in an event or a notification.
- A user only ever reads or marks their own notifications. Another user's id answers 404, the same as a missing one.
- IDs are strings in JSON. Page size is 20. Paging is newest first, with `?cursor=<id>` meaning "older than this id".
- `notifications.id` is `BIGINT AUTO_INCREMENT`. *(Plan change from the spec: rows are created in the worker, and a second snowflake generator there would need its own `NODE_ID` to avoid collisions with the server.)*
- Project rule (CLAUDE.md §5): every task updates its docs in the same commit. Module pages are in `docs/modules/`, and routes go in `api/openapi.yaml` and the routes table in `docs/architect.md`.
- Commit messages: conventional (`feat(...)`, `docs(...)`), ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Cross-user access.** User B calling `POST /api/notifications/{A's id}/read` gets 404 and A's row stays unread; B's list never contains A's rows. *(Task 1 repo test, Task 2 handler test.)*
2. **RabbitMQ redelivers a message**, or two scanner ticks publish the same link. The table ends up with exactly one row. *(Task 1 `Insert` duplicate test, Task 3 consumer test.)*
3. **Admin disables an already-disabled link.** No event is published. *(Task 5 test.)*
4. **Expiry edge cases.** A link that is disabled, whose owner is disabled, that has already expired, that is more than 24h away, that was created with 24h or less to live, or that already has its reminder: none of these is returned by `ExpiringLinks`. *(Task 1 repo test.)*
5. **Broker down during an admin action or a password reset.** The HTTP call still succeeds, and for a reset the email is still sent. *(Task 5 and Task 6 tests with a failing publisher.)*

---

### Task 1: Table, service types and MySQL repository

**Files:**
- Create: `deploy/migrations/009_create_notifications.up.sql`, `deploy/migrations/009_create_notifications.down.sql`
- Create: `internal/service/notification.go`
- Create: `internal/repository/notification_mysql.go`
- Create: `internal/repository/notification_integration_test.go`
- Modify: `internal/repository/integration_test.go` (the explicit migration list in `setupMySQL`, around line 64: add `009_create_notifications.up.sql`)
- Docs: `docs/modules/database.md` (new migration, new table), `docs/modules/repository.md`, `docs/architect.md` §8 MySQL tables

**Interfaces:**
- Produces (in package `service`, file `notification.go`):

```go
const (
	NotificationLinkDisabled    = "link_disabled"
	NotificationLinkEnabled     = "link_enabled"
	NotificationLinkExpiring    = "link_expiring"
	NotificationPasswordChanged = "password_changed"

	RouteLinkDisabled    = "link.disabled"
	RouteLinkEnabled     = "link.enabled"
	RouteLinkExpiring    = "link.expiring"
	RoutePasswordChanged = "user.password_changed"

	ExpiryWarningWindow   = 24 * time.Hour
	NotificationPageSize  = 20
)

// NotificationEvent is the JSON body of every notification event.
type NotificationEvent struct {
	DedupeKey  string     `json:"dedupe_key"`
	UserID     int64      `json:"user_id"`
	LinkID     int64      `json:"link_id,omitempty"`    // 0 for password_changed
	ShortCode  string     `json:"short_code,omitempty"` // "" for password_changed
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // link.expiring only
	OccurredAt time.Time  `json:"occurred_at"`
}

// NotificationTypeForRoute maps a routing key to its type; ok=false if unknown.
func NotificationTypeForRoute(routingKey string) (typ string, ok bool)

type Notification struct {
	ID        int64
	UserID    int64
	Type      string
	LinkID    *int64  // nil for password_changed
	ShortCode *string // nil for password_changed
	ExpiresAt *time.Time
	ReadAt    *time.Time
	CreatedAt time.Time
}

// ExpiringLink is a link the scanner should warn about.
type ExpiringLink struct {
	ID        int64
	UserID    int64
	ShortCode string
	ExpiresAt time.Time
}

type NotificationRepository interface {
	// Insert stores n (ID and CreatedAt are ignored, set by MySQL). A row with
	// the same dedupeKey already present is not an error and changes nothing.
	Insert(ctx context.Context, n *Notification, dedupeKey string) error
	// ListByUser returns up to limit rows of userID, newest first, with id < beforeID (0 = from the newest).
	ListByUser(ctx context.Context, userID, beforeID int64, limit int) ([]*Notification, error)
	CountUnread(ctx context.Context, userID int64) (int64, error)
	// MarkRead sets read_at if it is empty. ErrNotFound if id is missing or not userID's.
	MarkRead(ctx context.Context, userID, id int64) error
	MarkAllRead(ctx context.Context, userID int64) error
	// ExpiringLinks returns links with now < expires_at <= now+window,
	// expires_at - created_at > window, link and owner not disabled, and no
	// notification with dedupe_key "expiring:<id>". At most limit rows.
	ExpiringLinks(ctx context.Context, now time.Time, window time.Duration, limit int) ([]ExpiringLink, error)
}
```

- Produces (in package `repository`): `func NewNotificationMySQL(db *sqlx.DB) service.NotificationRepository`

- [ ] **Step 1: Write the migration.** Columns as in the spec's Database table, except `id BIGINT NOT NULL AUTO_INCREMENT`. Keys: `PRIMARY KEY (id)`, `UNIQUE KEY uk_dedupe (dedupe_key)`, `KEY idx_user_id (user_id, id)`, `KEY idx_user_unread (user_id, read_at)`. In the same up file, add `KEY idx_expires_at (expires_at)` on `links`, so the scanner does not scan the whole table. The down file drops both. Use `DATETIME` for new time columns (matches 006). Add a header comment saying what and why, like 006.

- [ ] **Step 2: Write failing integration tests** in `notification_integration_test.go`, using the existing `setupMySQL(t)`:
  - `TestNotificationMySQL_InsertDedupe`: Insert the same dedupeKey twice. No error, and `ListByUser` returns 1 row.
  - `TestNotificationMySQL_ListNewestFirstAndPaging`: insert 3 rows for user 1 and 1 row for user 2. `ListByUser(1, 0, 2)` returns the 2 newest of user 1, ids descending. `ListByUser(1, lastID, 2)` returns the remaining 1. User 2's row never appears.
  - `TestNotificationMySQL_PasswordChangedHasNullLink`: Insert with `LinkID == nil, ShortCode == nil`. Reading it back gives nil for both.
  - `TestNotificationMySQL_MarkRead`: `MarkRead(1, id)` sets `ReadAt` and `CountUnread(1)` drops by 1. `MarkRead(2, id)` returns `service.ErrNotFound` and the row stays unchanged. `MarkRead(1, 999999)` returns `ErrNotFound`. Marking twice is no error and keeps the first `read_at`.
  - `TestNotificationMySQL_MarkAllRead`: marks only that user's rows. `CountUnread` becomes 0 for them, and the other user is unchanged.
  - `TestNotificationMySQL_ExpiringLinks`: insert users and links directly with SQL, fixing `now`. Expect exactly link A (expires in 12h, created 10 days ago). Not returned: B (expires in 30h), C (expired 1h ago), D (expires in 12h but created 20h before expiry), E (A-like but `disabled_at` set), F (A-like, owner `users.disabled_at` set), G (A-like, but a notification row with `dedupe_key = 'expiring:<G.id>'` exists).

- [ ] **Step 3: Run them and see them fail.** Run `go test ./internal/repository/ -run TestNotificationMySQL -v` (needs Docker). Expected: compile failure, `NewNotificationMySQL` undefined.

- [ ] **Step 4: Write `service/notification.go` and `repository/notification_mysql.go`.** `Insert` uses `INSERT IGNORE`. `MarkRead` returns `ErrNotFound` when no row matches `id = ? AND user_id = ?`; it uses `read_at = COALESCE(read_at, NOW())` so a second call still finds the row. `ExpiringLinks` uses `NOT EXISTS (SELECT 1 FROM notifications n WHERE n.dedupe_key = CONCAT('expiring:', l.id))` and a join on `users` for the owner check. Pass `now` from Go and do not use `NOW()`, so tests can fix the time.

- [ ] **Step 5: Run the tests and see them pass.** Run `go test ./internal/repository/ -v`. Expected: PASS, including the old tests.

- [ ] **Step 6: Update the docs and commit.** `feat(notifications): add notifications table and repository`.

---

### Task 2: Notification service, HTTP API and server wiring

**Files:**
- Create: `internal/service/notification_service.go`, `internal/service/notification_service_test.go`
- Create: `internal/handler/notification.go`, `internal/handler/notification_handler_test.go`
- Modify: `cmd/server/main.go` (construct the repository, service and handler; `Register(mux, middleware.RequireAuth)`)
- Docs: `api/openapi.yaml` (3 routes and the `Notification` schema), the routes table in `docs/architect.md`, `docs/modules/service.md`, `docs/modules/handler.md`, `docs/modules/server.md`, `docs/spec.md` (requirement status)

**Interfaces:**
- Consumes: `service.NotificationRepository`, `service.Notification`, `NotificationPageSize` (Task 1).
- Produces:

```go
// package service
func NewNotificationService(repo NotificationRepository) *NotificationService
// List returns one page (NotificationPageSize) older than beforeID (0 = newest) and the user's unread count.
func (s *NotificationService) List(ctx context.Context, userID, beforeID int64) ([]*Notification, int64, error)
func (s *NotificationService) MarkRead(ctx context.Context, userID, id int64) error // ErrNotFound passes through
func (s *NotificationService) MarkAllRead(ctx context.Context, userID int64) error

// package handler
type NotificationServicer interface { List(...); MarkRead(...); MarkAllRead(...) } // same signatures as above
func NewNotificationHandler(svc NotificationServicer) *NotificationHandler
func (h *NotificationHandler) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler)
```

Routes: `GET /api/notifications`, `POST /api/notifications/{id}/read`, `POST /api/notifications/read-all`. The response of `GET`:

```json
{
  "items": [{"id":"12","type":"link_expiring","link_id":"734...","code":"abc","expires_at":"...","read_at":null,"created_at":"..."}],
  "next_cursor": "12",
  "unread_count": 3
}
```

`link_id` and `code` are `null` for `password_changed`. `next_cursor` is `""` when the page has fewer than 20 items, the same as `/api/links`.

- [ ] **Step 1: Write failing handler tests** with a mock `NotificationServicer`, in the style of `admin_handler_test.go`:
  - `TestNotifications_List`: the JSON matches the shape above. IDs are strings. A `password_changed` item has `"link_id":null,"code":null`. `next_cursor` is set when the mock returns 20 items and `""` when it returns 3.
  - `TestNotifications_ListBadCursor`: `?cursor=abc` returns 400.
  - `TestNotifications_MarkRead`: 204. The mock receives the caller's userID from the context and the parsed id.
  - `TestNotifications_MarkReadNotFound`: the mock returns `service.ErrNotFound` and the handler answers 404. A non-numeric id also answers 404.
  - `TestNotifications_ReadAll`: 204.
  - `TestNotifications_RequireAuth`: with no user in the context, each route answers 401.
- [ ] **Step 2: Write a failing service test.** `TestNotificationService_List` uses a fake repository: it passes `NotificationPageSize` and `beforeID`, and returns the unread count.
- [ ] **Step 3: Run them and see them fail.** Run `go test ./internal/handler/ ./internal/service/ -run Notification -v`. Expected: compile failure.
- [ ] **Step 4: Implement the service, the handler and the wiring in `main.go`.**
- [ ] **Step 5: Run the tests and see them pass.** Run `go test ./internal/... && go vet ./...`. Expected: PASS.
- [ ] **Step 6: Update the docs and commit.** `feat(notifications): add /api/notifications routes`.

---

### Task 3: Queue topology and notification consumer in the worker

**Files:**
- Modify: `internal/messaging/setup.go`: add the constants `QueueNotifications = "notifications"` and `QueueNotificationsDLQ = "notifications.dlq"`. Declare `notifications.dlq` and bind it to `dlxName` with routing key `notifications.dead`. Declare `notifications` with `x-dead-letter-exchange: dlxName` and `x-dead-letter-routing-key: notifications.dead`. Bind it to `Exchange` once for each of the 4 routing keys (use the `service.Route*` constants).
- Modify: `internal/messaging/consumer.go`: change `NewDLQConsumer(ch, log)` to `NewDLQConsumer(ch *amqp.Channel, queue string, log *zap.Logger)` and consume `queue`. The consumer tag becomes `queue + "-consumer"`.
- Create: `internal/messaging/notification_consumer.go`, `internal/messaging/notification_consumer_test.go`
- Modify: `cmd/worker/main.go`: build `repository.NewNotificationMySQL(db)`, open one channel for the notification consumer and one for a second `DLQConsumer` on `QueueNotificationsDLQ`. Update the existing DLQ call to pass `QueueDLQ`. Raise the `n` passed to `waitForShutdown` from 2 to 4.
- Docs: `docs/modules/messaging.md`, `docs/modules/worker.md`, `docs/architect.md` §8 RabbitMQ

**Interfaces:**
- Consumes: `service.NotificationRepository`, `NotificationEvent`, `NotificationTypeForRoute`, `Route*` (Task 1).
- Produces:

```go
func NewNotificationConsumer(ch *amqp.Channel, repo service.NotificationRepository, log *zap.Logger) *NotificationConsumer
func (c *NotificationConsumer) Start(ctx context.Context) error // blocks like ClickConsumer.Start
// handle settles one delivery; split out so tests need no broker.
func (c *NotificationConsumer) handle(ctx context.Context, d amqp.Delivery)
```

`handle` works like this:
- An unknown routing key, bad JSON, `user_id == 0` or an empty `dedupe_key` means `Nack(false, false)`, which sends the message to the DLQ.
- For the three link types, `link_id == 0` or an empty `short_code` is also rejected the same way.
- Otherwise it builds a `Notification`: `LinkID` and `ShortCode` are nil for `password_changed`, and `ExpiresAt` comes from the event.
- `Insert` OK means `Ack`. An `Insert` error means `Nack(false, !d.Redelivered)`, the same retry-once rule as clicks.

- [ ] **Step 1: Write failing tests** with `fakeAck` (already in `consumer_test.go`) and a fake repository:
  - `TestNotificationConsumer_InsertsAndAcks`: one case for each of the 4 routing keys. The inserted `Type`, `UserID`, `LinkID`, `ShortCode` and dedupe key are correct. `password_changed` has nil link fields. Each message is acked.
  - `TestNotificationConsumer_RejectsBadMessages`: bad JSON, an unknown routing key, a missing `user_id`, and `link.disabled` with no `link_id`. Each one gets a single Nack with `requeue == false` and no Insert.
  - `TestNotificationConsumer_RetriesOnceThenDLQ`: the repository fails. The first delivery is Nacked with requeue true; a delivery with `Redelivered: true` is Nacked with requeue false.
  - `TestNotificationConsumer_DuplicateIsAcked`: the repository returns nil for a duplicate (it is `INSERT IGNORE`). The message is acked, and that is the only settle.
- [ ] **Step 2: Run them and see them fail.** Run `go test ./internal/messaging/ -run Notification -v`.
- [ ] **Step 3: Implement the consumer and the topology, change `NewDLQConsumer`, and wire the worker.** Set Qos to prefetch 50 on the consumer channel.
- [ ] **Step 4: Run the tests and see them pass.** Run `go test ./internal/messaging/ ./cmd/worker/ && go build ./...`. Expected: PASS. Update `cmd/worker/main_test.go` only if it depends on the consumer count.
- [ ] **Step 5: Update the docs and commit.** `feat(worker): consume notification events into MySQL`.

---

### Task 4: Expiry scanner in the worker

**Files:**
- Create: `internal/service/expiry_scanner.go`, `internal/service/expiry_scanner_test.go`
- Modify: `config/config.go`: add `ExpiryScanInterval time.Duration \`envconfig:"EXPIRY_SCAN_INTERVAL" default:"5m"\``
- Modify: `cmd/worker/main.go`: create `messaging.NewPublisher(cfg.DialRabbitMQ, log)` (with `defer pub.Close()`), build the scanner, run `scanner.Run(ctx, cfg.ExpiryScanInterval)` in its own goroutine, and make `n` 5.
- Docs: `.env.example` (`EXPIRY_SCAN_INTERVAL=5m` with a one-line comment), `docs/modules/config.md`, `docs/modules/worker.md`, `docs/modules/service.md`

**Interfaces:**
- Consumes: `NotificationRepository.ExpiringLinks`, `ExpiryWarningWindow`, `RouteLinkExpiring`, `NotificationEvent` (Task 1). Also `Publisher` and `publishTimeout` (existing, in `link_service.go`).
- Produces:

```go
func NewExpiryScanner(repo NotificationRepository, pub Publisher, log *zap.Logger) *ExpiryScanner
// ScanOnce publishes link.expiring for each link ExpiringLinks returns (limit 500)
// and reports how many publishes succeeded. A failed publish is logged and
// skipped; the next scan retries it, since no notification row exists yet.
func (s *ExpiryScanner) ScanOnce(ctx context.Context, now time.Time) (published int, err error)
// Run calls ScanOnce right away and then on every tick, until ctx is done; then it returns nil.
// A ScanOnce error is logged, not returned, so a DB blip does not stop the worker.
func (s *ExpiryScanner) Run(ctx context.Context, interval time.Duration) error
```

Each event: `DedupeKey: "expiring:" + strconv.FormatInt(link.ID, 10)`, `UserID`, `LinkID`, `ShortCode`, `ExpiresAt`, `OccurredAt: now`. Each publish is synchronous, with its own `publishTimeout` context.

- [ ] **Step 1: Write failing tests** with a fake repository and a recording publisher:
  - `TestExpiryScanner_PublishesEachLink`: the repository returns 2 links. Two publishes go to `RouteLinkExpiring` with the exact dedupe keys and fields, `published == 2`, and the repository was called with `ExpiryWarningWindow` and limit 500.
  - `TestExpiryScanner_PublishFailureSkips`: the publisher fails on the first link. The second is still published and `published == 1`.
  - `TestExpiryScanner_RunStopsOnCancel`: `Run` with a 10ms interval scans at least once and returns nil soon after the cancel.
- [ ] **Step 2: Run them and see them fail.** Run `go test ./internal/service/ -run ExpiryScanner -v`.
- [ ] **Step 3: Implement the scanner, add the config field, and wire the worker.**
- [ ] **Step 4: Run the tests and see them pass.** Run `go test ./... && go build ./...` (the repository tests need Docker). Expected: PASS.
- [ ] **Step 5: Update the docs and commit.** `feat(worker): scan for links expiring within 24h`.

---

### Task 5: Admin disable/enable publishes events

**Files:**
- Create: `internal/service/publish.go`
- Modify: `internal/service/admin_service.go`: `NewAdminService` gains a final parameter `pub Publisher`, and `setLinkDisabled` publishes.
- Modify callers: `cmd/server/main.go` (pass `pub`), `cmd/server/admin_cli.go` (pass `service.NoopPublisher{}`), `internal/service/admin_service_test.go` (`newAdminSvc` helper)
- Docs: `docs/modules/service.md` (the admin section and the new events)

**Interfaces:**
- Consumes: `NotificationEvent`, `RouteLinkDisabled`, `RouteLinkEnabled` (Task 1). Also `AdminLink.ID`, `OwnerID` and `ShortCode` (existing).
- Produces (used again by Task 6):

```go
// publishAsync publishes in a goroutine with a fresh publishTimeout context and
// logs a warning (with the routing key) on failure, using logger.FromContext(ctx).
func publishAsync(ctx context.Context, pub Publisher, routingKey string, payload any)
```

In `setLinkDisabled`: keep the `changed` value returned by `repo.SetLinkDisabled`. After `GetLink` succeeds and `changed` is true, call `publishAsync` with `RouteLinkDisabled` or `RouteLinkEnabled`. The event has `DedupeKey` `"disabled:"+uuid.NewString()` or `"enabled:"+uuid.NewString()`, `UserID: link.OwnerID`, `LinkID: link.ID`, `ShortCode: link.ShortCode` and `OccurredAt: time.Now().UTC()`. Leave `LinkService.publish` as it is.

- [ ] **Step 1: Write failing tests** in `admin_service_test.go`, using a recording publisher that sends each call on a channel (the publish is async, so wait up to 2s, like `mockMailer.next`):
  - `TestAdminService_DisableLinkPublishes`: one `link.disabled` event with the owner's id, link id and code. The dedupe key starts with `disabled:`. The JSON of the payload does not contain the reason text.
  - `TestAdminService_EnableLinkPublishes`: the same, with `link.enabled` and `enabled:`.
  - `TestAdminService_NoEventWhenUnchanged`: the fake repository reports `changed == false`. No publish arrives within 100ms.
  - `TestAdminService_PublishFailureDoesNotFail`: the publisher returns an error. `DisableLink` still returns the link with no error.
- [ ] **Step 2: Run them and see them fail.** Run `go test ./internal/service/ -run AdminService -v`.
- [ ] **Step 3: Implement it and update all callers.**
- [ ] **Step 4: Run the tests and see them pass.** Run `go test ./... && go vet ./...`. Expected: PASS, including `cmd/server` admin CLI tests.
- [ ] **Step 5: Update the docs and commit.** `feat(admin): notify link owners on disable and enable`.

---

### Task 6: Password changed: event and email

**Files:**
- Modify: `internal/service/auth.go`: add a method to `Mailer`.
- Modify: `internal/service/auth_service.go`: `NewAuthService` gains a final parameter `pub Publisher`. `ResetPassword` publishes and mails at its end.
- Modify: `internal/mail/mail.go`, `internal/mail/mail_test.go`
- Modify callers: `cmd/server/main.go` (pass `pub`), the auth test helpers in `internal/service/auth_service_test.go` (`newAuthSvc`, `newAuthSvcWithMailer`, and `mockMailer` gains the new method), and any other `NewAuthService` or `Mailer` implementers that `go build ./...` finds.
- Docs: `docs/modules/mail.md`, `docs/modules/service.md` (auth section), `docs/spec.md`

**Interfaces:**
- Consumes: `publishAsync` (Task 5), `NotificationEvent`, `RoutePasswordChanged` (Task 1), and `runAsync` (existing).
- Produces:

```go
// service.Mailer
SendPasswordChanged(ctx context.Context, to string, changedAt time.Time, forgotURL string) error

// internal/mail
func (m *SMTPMailer) SendPasswordChanged(ctx context.Context, to string, changedAt time.Time, forgotURL string) error
func (m *LogMailer) SendPasswordChanged(ctx context.Context, to string, changedAt time.Time, forgotURL string) error
func passwordChangedMessage(from, to string, changedAt time.Time, forgotURL string) ([]byte, error)
```

- `forgotURL` is the reset URL with its last path segment replaced by `forgot-password` (`/reset-password` becomes `/forgot-password`, the GUI page at `src/app/[locale]/(auth)/forgot-password`), with no query. Compute it once in `NewAuthService` from `resetURL`.
- The message must carry:
  - the headers `From`, `To`, `Subject: Your password was changed`, `Date`, `MIME-Version`, and `Content-Type: text/plain; charset=UTF-8`;
  - a body that gives `changedAt` in UTC (RFC 1123) and says that if this was not you, you should reset your password now at `forgotURL`;
  - no token.
- Address validation is the same as `passwordResetMessage`: reuse it by pulling the address loop into a shared helper.
- `SMTPMailer.SendPasswordChanged` logs failures like `SendPasswordReset` does. `LogMailer` logs `to`.
- At the end of `ResetPassword`, after the second revoke succeeds and before the `DeleteLockout` line: call `publishAsync(ctx, s.pub, RoutePasswordChanged, NotificationEvent{DedupeKey: "password:"+uuid.NewString(), UserID: userID, OccurredAt: now})`. Then call `s.runAsync` to `SendPasswordChanged(user.Email, now, s.forgotURL)`. A failed send is logged only (the mailer already logs it).

- [ ] **Step 1: Write failing tests.**
  - `mail_test.go` `TestPasswordChangedMessage`: the message has the subject, the UTC time and the forgot URL, and no `token=`. An address containing `\r\n` is rejected.
  - `mail_test.go`: an SMTP send test for `SendPasswordChanged`, reusing the existing fake SMTP server in that file.
  - `auth_service_test.go` `TestResetPassword_NotifiesAndMails`: after a successful reset, the mock mailer gets `SendPasswordChanged` to the user's email, and the forgot URL ends with `/forgot-password`. The recording publisher gets one `user.password_changed` event with the user id, a dedupe key starting with `password:`, and `LinkID == 0`.
  - `TestResetPassword_InvalidTokenNoNotify`: a bad token sends no event and no mail within 100ms.
  - `TestResetPassword_PublishFailureStillMails`: the publisher fails. The reset succeeds and the mail is still sent.
- [ ] **Step 2: Run them and see them fail.** Run `go test ./internal/mail/ ./internal/service/ -v`.
- [ ] **Step 3: Implement it and update all callers.** `mockMailer` needs a second channel (or a kind field), so the existing reset-link tests still see only reset mails.
- [ ] **Step 4: Run the tests and see them pass.** Run `go vet ./... && go test ./... && make lint`. Expected: PASS.
- [ ] **Step 5: Update the docs and commit.** `feat(auth): notify and email on password change`. Also check `README.md`, `docs/deploy.md` and `docs/architect.md` §1 and §9 (the big picture and failure behaviour). Add the new flow and the rule "RabbitMQ down = notification lost, never a failed request". Also add the known gap: notifications are never deleted.
