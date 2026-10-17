package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestPasswordResetMessage(t *testing.T) {
	link := "https://app.example.com/reset-password?token=abc_123-XYZ"
	msg, err := passwordResetMessage("no-reply@example.com", "user@example.com", link)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(msg)
	for _, want := range []string{
		"From: no-reply@example.com\r\n",
		"To: user@example.com\r\n",
		"Subject: Reset your password\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
		link,
		"30 minutes",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("message missing %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "\r\n\r\n") {
		t.Error("headers and body must be separated by a blank CRLF line")
	}
}

func TestPasswordResetMessage_RejectsHeaderInjection(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		{"no-reply@example.com", "a@x.com\r\nBcc: z@y.com"},
		{"no-reply@example.com\nBcc: z@y.com", "a@x.com"},
		{"no-reply@example.com", "not an address"},
	} {
		if _, err := passwordResetMessage(tc.from, tc.to, "https://x/reset?token=t"); err == nil {
			t.Errorf("from %q to %q: want error, got nil", tc.from, tc.to)
		}
	}
}

func TestLogMailer_LogsLink(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	m := NewLogMailer(zap.New(core))

	if err := m.SendPasswordReset(context.Background(), "user@example.com", "https://x/reset?token=t"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log entry, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["to"] != "user@example.com" || fields["link"] != "https://x/reset?token=t" {
		t.Errorf("log fields = %v, want to and link", fields)
	}
}

// fakeSMTP accepts one plain-text SMTP session and returns the DATA it received.
func fakeSMTP(t *testing.T) (host string, port int, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { fmt.Fprintf(conn, "%s\r\n", s) } //nolint:errcheck // test server
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				data.WriteString(strings.TrimSpace(line) + "\n")
				say("250 ok")
			case cmd == "DATA":
				say("354 go ahead")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				ch <- data.String()
				return
			default:
				say("502 unknown")
			}
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port, ch
}

func TestSMTPMailer_Sends(t *testing.T) {
	host, port, got := fakeSMTP(t)
	m := NewSMTPMailer(SMTPConfig{Host: host, Port: port, From: "no-reply@example.com"}, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.SendPasswordReset(ctx, "user@example.com", "https://x/reset?token=t"); err != nil {
		t.Fatalf("send: %v", err)
	}
	s := <-got
	for _, want := range []string{"MAIL FROM:<no-reply@example.com>", "RCPT TO:<user@example.com>", "Subject: Reset your password", "https://x/reset?token=t"} {
		if !strings.Contains(s, want) {
			t.Errorf("session missing %q:\n%s", want, s)
		}
	}
}

func TestSMTPMailer_UnreachableServer(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	m := NewSMTPMailer(SMTPConfig{Host: "127.0.0.1", Port: 1, From: "no-reply@example.com"}, zap.New(core))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.SendPasswordReset(ctx, "user@example.com", "https://x/reset?token=t"); err == nil {
		t.Fatal("want error for unreachable server")
	}
	if logs.Len() != 1 {
		t.Errorf("want the failure logged once, got %d entries", logs.Len())
	}
}
