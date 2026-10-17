# `internal/mail`: sending email

**What it does.** Sends the emails the application needs. Today that is one
mail: the password reset link. It implements the `service.Mailer` interface.

**Where it sits.** `cmd/server` builds a mailer from the config and passes it to
`service.NewAuthService`. The service only knows the interface:

```go
type Mailer interface {
    SendPasswordReset(ctx context.Context, to, link string) error
}
```

Nothing else imports this package.

## Two implementations

| Type | Used when | What it does |
|---|---|---|
| `SMTPMailer` | `SMTP_HOST` is set | Sends through an SMTP relay |
| `LogMailer` | `SMTP_HOST` is empty | Writes `to` and `link` to the log at info level. Nothing is sent |

`LogMailer` exists so local development needs no mail setup: request a reset,
then copy the link from the server log.

## How `SMTPMailer` sends

1. Build the message (`passwordResetMessage`): plain text, UTF-8, subject
   "Reset your password", the link, and a note that it expires in 30 minutes
   and works once.
2. Dial `SMTP_HOST:SMTP_PORT` with the caller's context.
3. If the server offers **STARTTLS**, upgrade to TLS 1.2 or later. Port 587 with
   STARTTLS is what most providers (SES, Resend, Brevo, Gmail) expect.
4. If `SMTP_USERNAME` is set, authenticate with PLAIN. Go's `smtp.PlainAuth`
   refuses to send a password over an unencrypted connection unless the host
   is `localhost`, so a misconfigured relay fails instead of leaking it.
5. `MAIL FROM`, `RCPT TO`, `DATA`, `QUIT`.

`net/smtp` does not accept a context, so the context's deadline is set on the
connection and bounds the whole exchange. The service calls the mailer with a
10-second timeout.

**Failures are logged here**, at warn level with the recipient. The service sends
in a background goroutine and does not look at the returned error, so this log
line is the only trace of a mail that did not go out.

## Header injection

`to` comes from user input. A value such as `"a@x.com\r\nBcc: z@y.com"` would
add a header if it were copied into the message as-is. `passwordResetMessage`
accepts `from` and `to` only if `net/mail.ParseAddress` returns exactly the same
bare address, which rules out CR/LF, display names and anything else.

## Before you change it

- **New kind of mail:** add a method to `service.Mailer` (for example
  `SendPasswordChanged`), implement it on both types, and give it its own
  message builder with a test.
- **HTML mail** would need a `multipart/alternative` body; keep the plain-text
  part.
- Tests (`mail_test.go`, no Docker): the message builder, header-injection
  rejection, `LogMailer`'s log fields, a full send against an in-process fake
  SMTP server (no TLS, no auth), and logging when the server is unreachable.
  STARTTLS and auth against a real provider are checked by hand.
