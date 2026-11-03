# In-app notifications for link owners: design

Status: draft, waiting for approval. Nothing is built yet.
Covers two repos that sit side by side: `shorten-link` (Go API + worker) and
`shorten-link-gui` (Next.js front end). This file lives in both repos.

## Why

A link owner currently learns that a link stopped working only when someone
clicks it and gets an error. Two things silently kill a link:

- an admin disables it (`AdminService.DisableLink`), and
- it passes its `expires_at`.

The owner should hear about both, inside the app, and also hear when an admin
turns a link back on.

## Goals

- The owner of a link sees an in-app notification when:
  1. an admin **disables** their link,
  2. an admin **enables** it again,
  3. their link is **about to expire**: one notification, about 24 hours before
     `expires_at`. One reminder only, no second reminder.
- A user sees an in-app notification when **their password was changed**
  (today the only path is `AuthService.ResetPassword`, the forgot-password
  link). This one is about the account, not a link. The user **also gets an
  email** about it, at the account address.
- Events travel through RabbitMQ (pub/sub on the existing `shortlink` topic
  exchange). The user asked for this explicitly.
- The front end shows a bell with an unread count and a list. No realtime
  push; the client polls.

## Non-goals

- Email or any other channel for the link notifications. The one exception is
  the password-changed alert, which is both in-app and email.
- Notifying on user-level disable/enable (`DisableUser`/`EnableUser`). A
  disabled user is signed out and cannot read notifications, so it is noise.
- Notifying when an admin deletes a link, or when a link has already expired.
- Per-user notification settings, mute, or digest.
- WebSocket/SSE push.
- Deleting old notifications (retention). Known gap, see "Left to the builder".

## Anti-goals

- A flaky RabbitMQ must never make `DisableLink`/`EnableLink` fail or slow
  down. Disabling a link matters more than telling the owner.
- No duplicate notifications for the same event, even if RabbitMQ redelivers or
  the scanner runs twice.
- No leaking of data across users: a user must never read or mark another
  user's notification.

## How it works

```
admin disables link ─► AdminService ─publish link.disabled─►┐
admin enables link  ─► AdminService ─publish link.enabled──►│  exchange "shortlink"
password reset done ─► AuthService ─publish user.password_changed─►│
worker scanner (every few min) ─publish link.expiring──────►│  (topic)
                                                            ▼
                                              queue "notifications"
                                                            ▼
                                       worker NotificationConsumer
                                                            ▼
                                       MySQL table "notifications"
                                                            ▲
                        server: GET/POST /api/notifications │
                                                            ▲
                                       front end bell (polls)
```

### Events

Published with the existing `service.Publisher` (`Publish(ctx, routingKey,
payload)`). Routing keys: `link.disabled`, `link.enabled`, `link.expiring`,
`user.password_changed`. Payload (JSON):

| field         | meaning                                                       |
| ------------- | ------------------------------------------------------------- |
| `dedupe_key`  | idempotency key, see below                                    |
| `user_id`     | the recipient (link owner, or the account whose password changed) |
| `link_id`     | link id; absent for `user.password_changed`                   |
| `short_code`  | link code, so the UI can show it without another lookup; absent for `user.password_changed` |
| `expires_at`  | only for `link.expiring`                                      |
| `occurred_at` | when it happened                                              |

`dedupe_key` is `disabled:<new snowflake id>`, `enabled:<new snowflake id>` or
`password:<new snowflake id>` for one-off actions (each action is its own
event) and `expiring:<link_id>` for the reminder (at most one per link, ever).

### Publishing from the admin service

- `AdminService` gets a `Publisher` (the CLI passes `NoopPublisher`, like it
  already passes nil for the cache).
- `setLinkDisabled` currently throws away the `changed` value returned by the
  repository. It must use it: publish **only when `changed` is true**, so
  disabling an already-disabled link sends nothing.
- Publish after the DB commit, with a short timeout, like the click publisher
  does. If publishing fails, log a warning and carry on. **This is my call, not
  something the user chose:** the notification is lost in that case (no outbox).
  An outbox table would make it reliable but is much heavier; the user has not
  asked for it.

### Publishing from the auth service

- `AuthService` gets a `Publisher` too (wired in `cmd/server` with the same
  publisher; tests pass `NoopPublisher`).
- At the end of `ResetPassword`, after the password hash is saved and sessions
  are revoked (i.e. only on success), publish `user.password_changed` with the
  user id. Same rules as the admin events: short timeout, log a warning on
  failure, never fail the reset because of it.
- If a logged-in "change password" endpoint is added later, it should publish
  the same event. It does not exist today.

**Why also email:** an in-app alert alone is weak for this event. The usual
reason to alert on a password change is that someone else did it, and that
someone is the person who can now sign in and see the alert, while the real
owner is locked out. Email to the account address reaches the real owner. The
user chose to send both.

**The email:**

- Sent **directly by the server**, not through RabbitMQ: `AuthService` calls
  the existing `Mailer` in the background with its existing `runAsync`, the
  same way the reset link mail goes out. **My call:** this keeps the security
  alert working when RabbitMQ is down, and the worker needs no SMTP config.
- `service.Mailer` gets a second method, `SendPasswordChanged(ctx, to
  string) error`. `SMTPMailer` builds a plain-text message the same way
  `passwordResetMessage` does (same address checks against header injection).
  `LogMailer` logs it instead, for local runs without SMTP.
- Content: subject "Your password was changed"; the time it happened (UTC);
  "if this was not you, reset your password now" with the plain
  forgot-password page URL. **No token and no one-click link in this mail**:
  the user requests a fresh reset link themselves. Exact wording is the
  builder's.
- Not rate-limited separately: it is only sent after a successful reset, which
  already needs a single-use emailed token.
- A send failure is logged (as `SendPasswordReset` already does) and does not
  fail the reset.

### Expiry scanner (in the worker)

- A ticker in `cmd/worker` (interval from config, default 5 minutes; **my
  call**) runs a query for links where `expires_at` is within the next 24 hours
  and still in the future, `disabled_at IS NULL`, `expires_at - created_at > 24
  hours` (so a link created with a 1-hour expiry does not instantly ping its
  owner; **my call**), and no `notifications` row with
  `dedupe_key = 'expiring:<link_id>'` exists yet.
- For each, publish `link.expiring`. It is fine if the same link is published
  twice by two ticks before the consumer catches up: the consumer ignores the
  second one (see below). So the scanner needs no extra state and no new column
  on `links`.
- Scanner runs as one more goroutine counted in `waitForShutdown`. If several
  workers run, they may both publish; the dedupe key makes that harmless.

### Consumer (in the worker)

- New queue `notifications` bound to the four routing keys, with the same
  dead-letter setup the `clicks` queue has (dead letters go to a new
  `notifications.dlq`).
- On each message: insert a row into `notifications`. Use
  `INSERT ... ON DUPLICATE KEY` (or `INSERT IGNORE`) on the unique
  `dedupe_key`, so redelivery is a no-op. Ack after a successful insert; a bad
  payload is rejected to the DLQ. Same pattern as `ClickConsumer`.
- Dead-letter handling for the new DLQ: log it. Whether to reuse `DLQConsumer`
  or add a second instance is left to the builder.

### Database

New migration `009_create_notifications` (up and down):

| column       | type                | notes                                      |
| ------------ | ------------------- | ------------------------------------------ |
| `id`         | BIGINT              | snowflake from `pkg/idgen`, primary key    |
| `user_id`    | BIGINT NOT NULL     | recipient                                  |
| `type`       | VARCHAR(32) NOT NULL| `link_disabled`, `link_enabled`, `link_expiring`, `password_changed` |
| `link_id`    | BIGINT NULL         | NULL for `password_changed`                |
| `short_code` | VARCHAR(16) NULL    | NULL for `password_changed`                |
| `expires_at` | DATETIME NULL       | only for `link_expiring`                   |
| `dedupe_key` | VARCHAR(64) NOT NULL| unique key `uk_dedupe`                     |
| `read_at`    | DATETIME NULL       |                                            |
| `created_at` | DATETIME NOT NULL   | default current timestamp                  |

Index `(user_id, id)` for the list, plus one on `(user_id, read_at)` or
equivalent for the unread count (builder may pick). No foreign keys, matching
the existing tables. The notification stores structured fields, **not** display
text, so the front end owns wording and translation.

The admin's `reason` text is **not** put in the notification (my call: it may be
an internal note). If the user wants owners to see it, that is a small
follow-up.

### API (server)

All routes need a signed-in user (`requireAuth`), and only ever touch that
user's rows. IDs are strings in JSON, like the rest of the API.

| route                              | does                                                     |
| ---------------------------------- | -------------------------------------------------------- |
| `GET /api/notifications`           | newest first, cursor paging like `/api/links`; returns `items`, `next_cursor`, `unread_count` |
| `POST /api/notifications/{id}/read`| set `read_at` if empty; 204. 404 if it is not the caller's |
| `POST /api/notifications/read-all` | mark all of the caller's unread as read; 204             |

Item shape: `id`, `type`, `link_id` (nullable), `code` (nullable),
`expires_at` (nullable), `read_at` (nullable), `created_at`. `link_id` and
`code` are null only for `password_changed`. Page size: follow what `/api/links` uses.

New layers follow the existing ones: `NotificationRepository` interface in
`internal/service`, MySQL implementation in `internal/repository`,
`NotificationHandler` with a consumer-side interface in `internal/handler`,
wired in `cmd/server`.

### Docs to update in the same change (repo rule)

`api/openapi.yaml`, routes table in `docs/architect.md` (and §5, §8 RabbitMQ and
MySQL tables), `docs/spec.md`, `docs/modules/` pages for messaging, worker,
service, handler, repository, database, config, and `docs/modules/mail.md` for
the new password-changed mail; `.env.example` for the scanner
interval and warning window if they are configurable; `docs/deploy.md` only if
run steps change (a new migration needs running).

## Front end (`shorten-link-gui`)

- New feature folder `src/features/notifications/` with the same layout as
  `features/links` (`model/`, `view-models/`, `components/`, `index.ts`).
  Follow the repo's `CLAUDE.md` and `DESIGN.md`; they win over this document.
- `model`: Zod schemas for the list response; TanStack Query hooks using
  `authedHttp`: list (infinite or first page, builder's call), mark read,
  mark all read. Poll the list every ~60 seconds (`refetchInterval`); do not
  poll in a background tab (TanStack's default).
- `components`: a bell button with an unread badge, in `SiteHeader` next to
  `AccountMenu`, shown only when signed in. It opens a popover (`popover.tsx`
  already exists) listing notifications, newest first, unread ones visually
  marked, with a "mark all as read" action. Empty and error states reuse
  `components/common`.
- Text lives in next-intl messages (run `pnpm i18n:check`): one message per
  type, taking the link code and, for expiring, the time left or the date.
- Clicking a link notification marks it read and goes to the links dashboard
  (`/`). Deep-linking to one link is not required. Clicking a
  `password_changed` notification only marks it read; its text tells the user
  to reset the password again if it was not them (wording is the builder's).
- The Zod schema must accept null `link_id`/`code` for `password_changed`.
- Tests with Vitest for the view model and the badge/count logic. Run
  `pnpm verify`.

## Order of work

1. Backend: migration, repository, service, consumer, scanner, publisher wiring,
   API, docs. Done and merged before the front end, so the front end has a real
   API to call.
2. Front end: feature, header bell, messages, tests.

Each repo gets its own plan (`superpowers:writing-plans`), the front end plan
in `shorten-link-gui/docs/superpowers/plans/`.

## Left to the builder

- Exact scanner interval and config variable names.
- Index choice for unread count; page size.
- Whether to reuse `DLQConsumer` for `notifications.dlq`.
- Retention: notifications are never deleted. Known gap; mention it in the docs.
- Exact visual design of the bell and popover.
