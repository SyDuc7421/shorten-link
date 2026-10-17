# Password Reset Design

**Date:** 2026-10-17
**Requirement:** FR-A9 "User can reset a forgotten password" (and the core of FR-A11, revoking all sessions)
**Repos:** `shorten-link` (Go API) and `shorten-link-gui` (Next.js). The same file lives in both:
`docs/superpowers/specs/` in the API repo and `docs/specs/` in the GUI repo.

---

## 1. Goal

A user who forgot their password enters their email, receives a link, opens it,
and sets a new password. Afterwards they sign in with the new password, and every
existing session is ended.

**Success means:**

- Only someone who can read the account's mailbox can reset its password.
- A reset link works once and expires after 30 minutes.
- Responses and timing do not reveal whether an email is registered.
- After a reset, refresh tokens issued before it are rejected.

**Out of scope:** a "your password was changed" notification email; changing the
password while signed in; email verification at registration (FR-A10); logging out a
single session (FR-A8).

## 2. Decisions

| Question                           | Decision                                                                                               | Why                                                                                                                                                                |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| How is mail sent?                  | SMTP, configured by environment variables. With `SMTP_HOST` empty, the link is only written to the log | Works with any provider (SES, Resend, Brevo, Gmail, Mailtrap). Local development needs no mail setup                                                               |
| Where does the link point?         | The GUI: `PASSWORD_RESET_URL?token=<token>`                                                            | The API stays JSON-only; the GUI owns all pages                                                                                                                    |
| What happens to existing sessions? | All of the user's refresh tokens are revoked. The user signs in again                                  | A reset after a leaked password must lock the attacker out. Access tokens already issued stay valid for at most 15 minutes                                         |
| How is the reset token stored?     | Random 32 bytes; only its SHA-256 is stored in Redis, 30-minute TTL, consumed with `GETDEL`            | Same storage as refresh tokens, no migration, single use even under concurrency, a Redis dump cannot be replayed                                                   |
| When is the mail sent?             | In a background goroutine with a 10 s timeout; the endpoint returns `202` immediately                  | Sending inside the request would make registered emails measurably slower than unknown ones. A mail in flight during a server restart is lost; the user asks again |

Rejected: a stateless signed JWT (long link, cannot revoke one link, every link stays
valid until the password changes); a MySQL `password_resets` table (migration, cleanup
and single-use logic for no benefit at this scale); sending through RabbitMQ and the
worker (more robust, more work; revisit if mail loss becomes a problem).

## 3. API (`shorten-link`)

### `POST /api/auth/forgot-password`

Request: `{"email": "user@example.com"}`

| Status            | When                                                                    |
| ----------------- | ----------------------------------------------------------------------- |
| `202`, empty body | Always when the request is well formed, whether or not the email exists |
| `400`             | Missing or empty `email`, or invalid JSON                               |
| `413`             | Body over 1 MB                                                          |
| `429`             | More than 5 requests per hour from one IP                               |

When the email belongs to a user:

1. Skip everything if `prt-cooldown:<email>` exists; otherwise set it for 60 s. One
   mail per minute per address stops someone flooding a victim's inbox. The response
   is still `202`.
2. Generate the token, store its hash, and invalidate the user's previous reset token.
3. Send the mail in the background.

The email is normalised (trimmed, lower-cased) exactly as login does.

### `POST /api/auth/reset-password`

Request: `{"token": "<token from the link>", "password": "<new password>"}`

| Status | When                                                                     |
| ------ | ------------------------------------------------------------------------ |
| `204`  | Password changed. The user must sign in again                            |
| `400`  | Missing `token` or `password`, invalid JSON, or password not 8–72 bytes  |
| `401`  | Token unknown, expired or already used: `invalid or expired reset token` |
| `413`  | Body over 1 MB                                                           |
| `429`  | More than 10 requests per minute from one IP                             |

Order of operations:

1. Check the password length **first**, so a weak password does not burn the token.
2. `GETDEL prt:<sha256(token)>` → user ID. Missing → `401`.
3. Load the user (`GetByID`, needed for the email in step 5). bcrypt the new password
   (cost 12, as registration) and update `users.password_hash`.
4. Revoke all refresh tokens of the user.
5. Clear `lockout:<email>` and delete `prt-user:<userID>`.

If step 3 or 4 fails, the token is already gone and the API answers `500`; the user
requests a new link. Putting the token back would let a failing request be retried
forever with the same link, which is worse.

## 4. Backend design

### 4.1 New package `internal/mail`

The service declares what it needs, next to its other interfaces:

```go
// Mailer sends account emails. Implemented by internal/mail.
type Mailer interface {
    SendPasswordReset(ctx context.Context, to, link string) error
}
```

Two implementations:

- **`SMTPMailer`**: `net/smtp`, STARTTLS when the server offers it (port 587), PLAIN
  auth when a username is set. Plain-text English mail, subject "Reset your password",
  body with the link and "This link expires in 30 minutes. If you did not ask for it,
  ignore this email." Logs failures itself.
- **`LogMailer`**: logs `to` and `link` at info level. Used when `SMTP_HOST` is empty.

`cmd/server` picks one from the config.

### 4.2 Configuration

| Variable                         | Default                                | Notes                              |
| -------------------------------- | -------------------------------------- | ---------------------------------- |
| `PASSWORD_RESET_URL`             | `http://localhost:3000/reset-password` | GUI page; `?token=` is appended    |
| `SMTP_HOST`                      | empty                                  | Empty = `LogMailer`                |
| `SMTP_PORT`                      | `587`                                  |                                    |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | empty                                  | No auth when the username is empty |
| `SMTP_FROM`                      | `no-reply@localhost`                   | Sender address                     |

Added to `.env.example` with comments, and to `docs/deploy.md`.

### 4.3 Service (`AuthService`)

- `NewAuthService` gains `mailer Mailer` and `resetURL string`.
- `RequestPasswordReset(ctx, email) error`: returns an error only for storage failures.
- `ResetPassword(ctx, token, password) error`: returns `ErrWeakPassword`,
  `ErrInvalidToken` or a wrapped storage error.
- Mail is sent with a helper shaped like `LinkService.publishAsync`: a goroutine with
  its own 10 s timeout, error ignored by the service.
- Constants: `resetTokenTTL = 30 * time.Minute`, `resetCooldown = time.Minute`.

### 4.4 Storage

`UserRepository` gains `UpdatePasswordHash(ctx, userID, hash) error`
(`ErrNotFound` if no row changed).

`TokenCache` gains:

| Method                                                 | Redis                                                                                                                            |
| ------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| `StorePasswordReset(ctx, tokenHash, userID, ttl)`      | Delete the hash in `prt-user:<userID>` (if any), then `SET prt:<hash> <userID> EX ttl` and `SET prt-user:<userID> <hash> EX ttl` |
| `ConsumePasswordReset(ctx, tokenHash) (userID, error)` | `GETDEL prt:<hash>`; `redis.Nil` → `ErrNotFound`                                                                                 |
| `AcquireResetCooldown(ctx, email, ttl) (bool, error)`  | `SET prt-cooldown:<email> 1 NX EX ttl`; true when set                                                                            |
| `DeletePasswordReset(ctx, userID)`                     | `DEL prt-user:<userID>`                                                                                                          |
| `RevokeAllRefreshTokens(ctx, userID)`                  | `SMEMBERS urt:<userID>`, delete every `rt:<jti>`, then `DEL urt:<userID>`                                                        |

Session index, so all of a user's refresh tokens can be found:

- `StoreRefreshToken` also runs `SADD urt:<userID> <jti>` and `EXPIRE urt:<userID> 7d`.
- `ConsumeRefreshToken` also runs `SREM urt:<userID> <jti>` after a successful `GETDEL`.
- Entries for tokens that expired on their own may linger in the set until it expires.
  They point at keys that no longer exist, so deleting them is harmless.

**Rollout note:** refresh tokens issued before this ships are not in `urt:` and survive
a reset until they expire (at most 7 days). Accepted; not worth forcing everyone to
sign in again.

### 4.5 Handlers and server

- `AuthHandler` gains `ForgotPassword` and `ResetPassword`, registered in `Register`.
  `AuthServicer` gains the two service methods.
- `withRateLimits`: `forgot-password` 5/hour, `reset-password` 10/minute.

## 5. GUI design (`shorten-link-gui`)

### 5.1 Routes and entry point

- `/forgot-password` and `/reset-password?token=…` under `app/[locale]/`
  (`localePrefix: 'as-needed'` keeps them unprefixed).
- The sign-in form shows a "Forgot password?" link beside the password label
  (sign-in mode only).

### 5.2 Code layout (`src/features/auth/`, existing MVVM pattern)

| Layer       | Files                                                                                                                     |
| ----------- | ------------------------------------------------------------------------------------------------------------------------- |
| model       | `password-reset-api.ts` (`useForgotPasswordMutation`, `useResetPasswordMutation`); error mapping next to `auth-errors.ts` |
| view-models | `use-forgot-password-view-model.ts`, `use-reset-password-view-model.ts`                                                   |
| components  | `forgot-password-form.tsx`, `reset-password-form.tsx`, exported from `index.ts`                                           |
| pages       | `app/[locale]/forgot-password/page.tsx`, `app/[locale]/reset-password/page.tsx`, laid out like the sign-in page           |

All user-facing text goes in `messages/en.json` under `auth`.

### 5.3 `/forgot-password` states

| State                   | UI                                                                                                                                                                          |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Form                    | Email field, submit button, "Back to sign in" link                                                                                                                          |
| Submitting              | Submit disabled with spinner                                                                                                                                                |
| Sent (`202`)            | "If an account exists for {email}, we've sent a reset link. It expires in 30 minutes." Resend button and link back to sign in. Same message whether or not the email exists |
| `429`                   | Existing `auth.tooManyAttempts`                                                                                                                                             |
| Network or server error | Existing `errors.network` / `errors.server`                                                                                                                                 |

A signed-in user is redirected home, as on the sign-in page.

### 5.4 `/reset-password` states

| State                  | UI                                                                                                                                         |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| No token in the URL    | "This reset link is invalid" with a "Request a new link" button to `/forgot-password`                                                      |
| Form                   | New password and confirmation, show/hide toggle, 8–72 hint; mismatch checked on the client                                                 |
| Submitting             | Submit disabled with spinner                                                                                                               |
| Done (`204`)           | "Password updated. Sign in with your new password." Button to `/login`. A local session, if any, is signed out (the server has revoked it) |
| `401`                  | "This link is invalid or has expired." Button to request a new one                                                                         |
| `400`                  | Error on the password field (`auth.weakPassword`)                                                                                          |
| `429`, network, server | As above                                                                                                                                   |

### 5.5 Protecting the token in the URL

- The page metadata sets `referrer: 'no-referrer'`, so the token never leaks in a
  `Referer` header.
- After reading the token, the view-model removes it from the address bar
  (`router.replace` without the query) and keeps it in memory only.

## 6. Testing

**API** (tests first):

- Service, with mocks: unknown email stores and sends nothing; known email stores only
  the hash and the mailed link carries a token whose SHA-256 equals it; cooldown
  suppresses the mail; weak password returns `ErrWeakPassword` without consuming the
  token; bad token returns `ErrInvalidToken`; success updates the hash (bcrypt matches
  the new password), revokes all sessions and clears the lockout.
- Repository, against real Redis and MySQL: a reset token works once; a new token
  invalidates the previous one; the cooldown; `urt:` is maintained on store and
  consume; after `RevokeAllRefreshTokens` an old refresh token is rejected;
  `UpdatePasswordHash`.
- Handler: `202` for any well-formed email; every status in section 3.
- Rate limits: two new cases in `cmd/server/ratelimit_test.go`.
- Mail: the message builder (recipient, subject, link) and `LogMailer`. Real SMTP is
  checked by hand.

**GUI:** vitest for both view-models, one test per state in 5.3 and 5.4, and for the
error mapping. `pnpm verify` passes.

**End to end, by hand** (API and GUI running locally, `SMTP_HOST` empty):

1. Request a link, copy it from the API log, set a new password.
2. Sign in with the new password works; the old password is rejected.
3. A refresh token issued before the reset returns `401`.
4. Opening the same link a second time ends in `401`.

## 7. Documentation (same commits as the code)

- API: `api/openapi.yaml`; `docs/spec.md` (FR-A9 done, FR-A11 "revoked on password
  reset"); `docs/architect.md` (routes, Redis keys); `docs/modules/` pages `service`,
  `repository`, `handler`, `config`, `server`, and a new `mail.md`; `.env.example`;
  `docs/deploy.md`.
- GUI: `messages/en.json`; `README.md` if it lists routes.
