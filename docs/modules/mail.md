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
2. Dial `SMTP_HOST:SMTP_PORT` with the caller's context. On **port 465** the
   connection is TLS from the first byte ("implicit TLS").
3. On any other port, upgrade with **STARTTLS** (TLS 1.2 or later). If the
   server does not offer STARTTLS, **refuse to send** unless the host is
   `localhost` or a loopback address. A man in the middle can delete the
   STARTTLS offer from the server's reply; sending anyway would put the reset
   link on the wire in clear text. Port 587 with STARTTLS or 465 with TLS is
   what most providers (SES, Resend, Brevo, Gmail) offer.
4. If `SMTP_USERNAME` is set, authenticate with PLAIN.
5. `MAIL FROM`, `RCPT TO`, `DATA`, `QUIT`.

`net/smtp` does not accept a context, so the context's deadline is set on the
connection and bounds the whole exchange. The service runs the whole reset
request, lookup and mail included, in a background goroutine with a 10-second
timeout.

**Failures are logged here**, at warn level with the recipient, and the service
logs the returned error again at error level with the request ID. Nobody is
waiting for the result, so these log lines are the only trace of a mail that
did not go out.

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
- Tests (`mail_test.go`, no Docker) run against an in-process fake SMTP server
  using `httptest`'s self-signed certificate: plain text to loopback, STARTTLS
  upgrade, refusal when a remote host offers no STARTTLS, implicit TLS on 465,
  and logging when the server is unreachable. Also the message builder,
  header-injection rejection and `LogMailer`. Two unexported fields
  (`dialAddr`, `rootCAs`) exist only so tests can point at the fake server and
  trust its certificate. Auth against a real provider is checked by hand.
