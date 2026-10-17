// Package mail sends account emails. It implements service.Mailer.
package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// SMTPConfig is the connection to an SMTP relay. Username empty means no auth.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

// SMTPMailer sends mail through an SMTP relay, upgrading to TLS with STARTTLS
// when the server offers it.
type SMTPMailer struct {
	cfg SMTPConfig
	log *zap.Logger
}

// NewSMTPMailer constructs an SMTPMailer.
func NewSMTPMailer(cfg SMTPConfig, log *zap.Logger) *SMTPMailer {
	return &SMTPMailer{cfg: cfg, log: log}
}

// SendPasswordReset mails link to to. Failures are logged here because callers
// send in the background and do not look at the error.
func (m *SMTPMailer) SendPasswordReset(ctx context.Context, to, link string) error {
	msg, err := passwordResetMessage(m.cfg.From, to, link)
	if err == nil {
		err = m.send(ctx, to, msg)
	}
	if err != nil {
		m.log.Warn("send password reset mail", zap.String("to", to), zap.Error(err))
	}
	return err
}

func (m *SMTPMailer) send(ctx context.Context, to string, msg []byte) error {
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	// net/smtp has no context support; the deadline bounds the whole exchange.
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline) //nolint:errcheck
	}

	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: smtp handshake: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("mail: starttls: %w", err)
		}
	}
	if m.cfg.Username != "" {
		// PlainAuth refuses to send the password over an unencrypted
		// connection, except to localhost.
		if err := c.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := c.Mail(m.cfg.From); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("mail: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: end body: %w", err)
	}
	return c.Quit()
}

// LogMailer writes the link to the log instead of sending mail. It is used when
// SMTP_HOST is empty, so local development needs no mail setup.
type LogMailer struct {
	log *zap.Logger
}

// NewLogMailer constructs a LogMailer.
func NewLogMailer(log *zap.Logger) *LogMailer {
	return &LogMailer{log: log}
}

// SendPasswordReset logs to and link.
func (m *LogMailer) SendPasswordReset(_ context.Context, to, link string) error {
	m.log.Info("password reset link (SMTP_HOST not set, mail not sent)",
		zap.String("to", to), zap.String("link", link))
	return nil
}

// passwordResetMessage builds the RFC 5322 message. from and to must be bare
// addresses; anything else (including CR/LF, which would inject headers) is
// rejected.
func passwordResetMessage(from, to, link string) ([]byte, error) {
	for _, a := range []string{from, to} {
		parsed, err := mail.ParseAddress(a)
		if err != nil || parsed.Address != a {
			return nil, fmt.Errorf("mail: invalid address %q", a)
		}
	}

	var b strings.Builder
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from)
	header("To", to)
	header("Subject", "Reset your password")
	header("Date", time.Now().UTC().Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=UTF-8")
	b.WriteString("\r\n")
	b.WriteString("Someone asked to reset the password for this account.\r\n\r\n")
	b.WriteString("Open this link to choose a new password:\r\n")
	b.WriteString(link + "\r\n\r\n")
	b.WriteString("This link expires in 30 minutes and works once.\r\n")
	b.WriteString("If you did not ask for it, ignore this email. Your password has not changed.\r\n")
	return []byte(b.String()), nil
}
