# Password Reset Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Users can request a reset link by email and set a new password, which ends all their sessions.

**Architecture:** The API stores a SHA-256 of a random token in Redis (30 min, single use), mails the link through SMTP in the background, and on reset updates the bcrypt hash and revokes every refresh token through a new per-user Redis set. The GUI adds `/forgot-password` and `/reset-password` pages in the existing MVVM auth feature.

**Tech Stack:** Go 1.26, `net/smtp`, go-redis v9, MySQL 8, testcontainers; Next.js 16, next-intl, react-hook-form + zod, TanStack Query, vitest.

**Spec:** `docs/superpowers/specs/2026-10-17-password-reset-design.md` (same file in the GUI repo under `docs/specs/`).

## Global Constraints

- Token: 32 random bytes, base64url without padding; Redis stores only the hex SHA-256.
- TTLs: reset token 30 min, per-email cooldown 60 s, mail send timeout 10 s.
- Rate limits per IP: `forgot-password` 5/hour, `reset-password` 10/minute.
- Password 8–72 bytes, bcrypt cost 12 (same as registration).
- `forgot-password` answers `202` with an empty body for every well-formed request.
- `reset-password`: `204`; `400` weak password or missing field; `401` `invalid or expired reset token`.
- Redis keys: `prt:<hash>`, `prt-user:<userID>`, `prt-cooldown:<email>`, `urt:<userID>`.
- Config: `PASSWORD_RESET_URL` (default `http://localhost:3000/reset-password`), `SMTP_HOST` (empty = log only), `SMTP_PORT` 587, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM` (default `no-reply@localhost`).
- Docs change in the same commit as the code (CLAUDE.md §5). GUI: no hardcoded text, design tokens only, `pnpm verify` passes.

## Review Focus

1. Email with different case or spaces (`" A@x.com "`) must reach the same account as login does. Test in Task 3.
2. A weak password on reset must not burn the token; the user retries with the same link. Test in Task 3.
3. Two resets with the same token at once: exactly one succeeds. Covered by `GETDEL`; test in Task 1.
4. Requesting a second link makes the first one invalid. Test in Task 1.
5. Opening the reset page without `?token=` must show the invalid-link state, not an empty form. Test in Task 7.

---

## API repo (`shorten-link`)

### Task 1: Redis and MySQL storage

**Files:** Modify `internal/repository/token_redis.go`, `internal/repository/user_mysql.go`, `internal/service/auth.go` (interfaces), `internal/service/auth_service_test.go` (mock gains the new methods), `internal/repository/integration_test.go`; docs `docs/modules/repository.md`, `docs/architect.md` (Redis keys).

**Interfaces produced:**
```go
// UserRepository
UpdatePasswordHash(ctx context.Context, userID int64, hash string) error // ErrNotFound if no row
// TokenCache
StorePasswordReset(ctx context.Context, tokenHash string, userID int64, ttl time.Duration) error
ConsumePasswordReset(ctx context.Context, tokenHash string) (int64, error) // ErrNotFound if missing
DeletePasswordReset(ctx context.Context, userID int64) error
AcquireResetCooldown(ctx context.Context, email string, ttl time.Duration) (bool, error)
RevokeAllRefreshTokens(ctx context.Context, userID int64) error
```
`StoreRefreshToken` also adds the jti to `urt:<userID>`; `ConsumeRefreshToken` removes it.

- [ ] Integration tests (fail first): reset token single use and parallel consume (only one of two goroutines gets the ID); second `StorePasswordReset` invalidates the first; cooldown true then false; revoke-all makes `ConsumeRefreshToken` return `ErrNotFound` for every earlier token; consume removes the jti from `urt:`; `UpdatePasswordHash` changes the row and returns `ErrNotFound` for an unknown ID.
- [ ] Implement, run `go test ./internal/repository/`, update docs, commit.

### Task 2: `internal/mail`

**Files:** Create `internal/mail/mail.go`, `internal/mail/mail_test.go`, `docs/modules/mail.md`; modify `docs/modules/README.md`, `docs/architect.md` §5.

**Interfaces produced:**
```go
type SMTPConfig struct{ Host string; Port int; Username, Password, From string }
func NewSMTPMailer(cfg SMTPConfig, log *zap.Logger) *SMTPMailer
func NewLogMailer(log *zap.Logger) *LogMailer
func (*SMTPMailer) SendPasswordReset(ctx context.Context, to, link string) error
func (*LogMailer) SendPasswordReset(ctx context.Context, to, link string) error
func passwordResetMessage(from, to, link string) []byte // RFC 5322 text, CRLF line endings
```
- [ ] Tests (fail first): message has `From`, `To`, `Subject: Reset your password`, the link, the 30-minute note; header injection in `to` (`"a@x.com\r\nBcc: z@y.com"`) is rejected with an error; `LogMailer` logs `to` and `link` (zap observer).
- [ ] Implement, test, docs, commit.

### Task 3: Service

**Files:** Modify `internal/service/auth.go` (`Mailer` interface), `internal/service/auth_service.go`, `internal/service/auth_service_test.go`, `cmd/server/main.go` (constructor call only, with `LogMailer` until Task 4); docs `docs/modules/service.md`.

**Interfaces produced:**
```go
type Mailer interface { SendPasswordReset(ctx context.Context, to, link string) error }
func NewAuthService(users UserRepository, tokens TokenCache, gen *idgen.Generator,
    privateKey *rsa.PrivateKey, publicKey *rsa.PublicKey, mailer Mailer, resetURL string) *AuthService
func (s *AuthService) RequestPasswordReset(ctx context.Context, email string) error
func (s *AuthService) ResetPassword(ctx context.Context, token, password string) error
```
- [ ] Tests (fail first): unknown email → no store, no mail; `" A@x.com "` → mail to `a@x.com`; stored hash = hex SHA-256 of the token in the mailed link, link starts with `resetURL?token=`; cooldown held → no store, no mail; weak password → `ErrWeakPassword` and the token is still consumable; unknown token → `ErrInvalidToken`; success → new password verifies with bcrypt, revoke-all called, lockout cleared, `prt-user` deleted.
- [ ] Implement, `go test ./internal/service/`, docs, commit.

### Task 4: HTTP, config, wiring

**Files:** Modify `internal/handler/auth.go`, `internal/handler/auth_handler_test.go`, `cmd/server/main.go`, `cmd/server/ratelimit_test.go`, `config/config.go`, `.env.example`, `api/openapi.yaml`, `docs/spec.md`, `docs/deploy.md`, `docs/architect.md` (routes), `docs/modules/handler.md`, `docs/modules/server.md`, `docs/modules/config.md`.

- [ ] Handler tests (fail first): forgot `202` empty body, `400` missing email, `500` on service error; reset `204`, `400` missing field, `400` on `ErrWeakPassword`, `401` on `ErrInvalidToken`.
- [ ] Rate-limit test cases: `POST /api/auth/forgot-password` 5, `POST /api/auth/reset-password` 10.
- [ ] Config fields with defaults; `main.go` picks `SMTPMailer` when `SMTP_HOST` is set, else `LogMailer`.
- [ ] Full `go test ./...`, `go vet ./...`, `make lint`; docs; commit.

## GUI repo (`shorten-link-gui`)

### Task 5: API model

**Files:** Create `src/features/auth/model/password-reset-api.ts`, `src/features/auth/model/password-reset-errors.ts`, `src/features/auth/model/password-reset-errors.test.ts`.

**Interfaces produced:**
```ts
export function useForgotPasswordMutation(): UseMutationResult<void, unknown, { email: string }>;
export function useResetPasswordMutation(): UseMutationResult<void, unknown, { token: string; password: string }>;
export type ResetFailure =
  | { field: 'root'; reason: 'invalidLink' | 'tooManyAttempts' | 'network' | 'server' }
  | { field: 'password'; reason: 'weakPassword' };
export function toForgotFailure(error: unknown): { reason: 'tooManyAttempts' | 'network' | 'server' };
export function toResetFailure(error: unknown): ResetFailure;
```
- [ ] Tests for both mappers (401 → invalidLink, 400 → weakPassword, 429, non-HttpError → network, other → server), implement, `pnpm test`, commit.

### Task 6: Forgot-password page

**Files:** Create `src/features/auth/view-models/use-forgot-password-view-model.ts` (+ `.test.tsx`), `src/features/auth/components/forgot-password-form.tsx`, `src/app/[locale]/forgot-password/page.tsx`; modify `src/features/auth/index.ts`, `src/features/auth/components/auth-form.tsx` ("Forgot password?" link in sign-in mode), `messages/en.json`.

- [ ] View-model tests: invalid email blocked; success shows the sent state with the email; 429 and network errors set the root message; signed-in user redirected home.
- [ ] Implement, `pnpm verify`, commit.

### Task 7: Reset-password page

**Files:** Create `src/features/auth/view-models/use-reset-password-view-model.ts` (+ `.test.tsx`), `src/features/auth/components/reset-password-form.tsx`, `src/app/[locale]/reset-password/page.tsx` (`referrer: 'no-referrer'`); modify `src/features/auth/index.ts`, `messages/en.json`, `README.md` if it lists routes.

- [ ] View-model tests: no token → invalid-link state; token removed from the URL after mount; mismatched confirmation blocked; 204 → done state and local session signed out; 401 → invalid-link state; 400 → password field error.
- [ ] Implement, `pnpm verify`, commit.

### Task 8: End-to-end check by hand

- [ ] Run API (`make run`, `SMTP_HOST` empty) and GUI (`pnpm dev`). Walk the four steps in spec §6. Record the result in the PR description.
