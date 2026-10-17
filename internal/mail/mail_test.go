package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type fakeOpts struct {
	cert        *tls.Certificate // required for implicitTLS and startTLS
	implicitTLS bool             // TLS from the first byte (port 465 style)
	startTLS    bool             // advertise and accept STARTTLS
}

// fakeSMTP accepts one SMTP session and returns what it received after MAIL,
// prefixed with "tls:" if the session was encrypted by then.
func fakeSMTP(t *testing.T, opts fakeOpts) (addr string, got <-chan string) {
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
		encrypted := false
		if opts.implicitTLS {
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*opts.cert}})
			encrypted = true
		}
		r := bufio.NewReader(conn)
		say := func(s string) { fmt.Fprintf(conn, "%s\r\n", s) } //nolint:errcheck // test server
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- "error: " + err.Error()
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				if opts.startTLS && !encrypted {
					say("250-fake")
					say("250 STARTTLS")
				} else {
					say("250 fake")
				}
			case cmd == "STARTTLS":
				say("220 ready")
				conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*opts.cert}})
				r = bufio.NewReader(conn)
				encrypted = true
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				if encrypted && strings.HasPrefix(cmd, "MAIL") {
					data.WriteString("tls:")
				}
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
	return ln.Addr().String(), ch
}

// testCert borrows httptest's self-signed certificate, valid for example.com
// and 127.0.0.1, and returns a pool that trusts it.
func testCert(t *testing.T) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	cert := srv.TLS.Certificates[0]
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	srv.Close()
	return &cert, pool
}

func sendVia(t *testing.T, m *SMTPMailer) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.SendPasswordReset(ctx, "user@example.com", "https://x/reset?token=t")
}

func TestSMTPMailer_Sends(t *testing.T) {
	addr, got := fakeSMTP(t, fakeOpts{})
	host, port := splitHostPort(t, addr)
	// Plain text is acceptable only because the relay is on loopback.
	m := NewSMTPMailer(SMTPConfig{Host: host, Port: port, From: "no-reply@example.com"}, zap.NewNop())

	if err := sendVia(t, m); err != nil {
		t.Fatalf("send: %v", err)
	}
	s := <-got
	for _, want := range []string{"MAIL FROM:<no-reply@example.com>", "RCPT TO:<user@example.com>", "Subject: Reset your password", "https://x/reset?token=t"} {
		if !strings.Contains(s, want) {
			t.Errorf("session missing %q:\n%s", want, s)
		}
	}
}

func TestSMTPMailer_UpgradesWithSTARTTLS(t *testing.T) {
	cert, pool := testCert(t)
	addr, got := fakeSMTP(t, fakeOpts{cert: cert, startTLS: true})
	m := NewSMTPMailer(SMTPConfig{Host: "example.com", Port: 587, From: "no-reply@example.com"}, zap.NewNop())
	m.dialAddr, m.rootCAs = addr, pool

	if err := sendVia(t, m); err != nil {
		t.Fatalf("send: %v", err)
	}
	if s := <-got; !strings.HasPrefix(s, "tls:") {
		t.Errorf("mail was sent before TLS was established:\n%s", s)
	}
}

// A man in the middle can strip STARTTLS from the server's reply. Sending
// anyway would put the reset link on the wire in clear text.
func TestSMTPMailer_RefusesPlaintextToRemoteHost(t *testing.T) {
	addr, got := fakeSMTP(t, fakeOpts{})
	m := NewSMTPMailer(SMTPConfig{Host: "smtp.example.com", Port: 587, From: "no-reply@example.com"}, zap.NewNop())
	m.dialAddr = addr

	if err := sendVia(t, m); err == nil {
		t.Fatal("want error when a remote relay does not offer STARTTLS")
	}
	select {
	case s := <-got:
		if strings.Contains(s, "token=") {
			t.Errorf("the link reached the server in clear text:\n%s", s)
		}
	case <-time.After(time.Second):
	}
}

func TestSMTPMailer_ImplicitTLSOnPort465(t *testing.T) {
	cert, pool := testCert(t)
	addr, got := fakeSMTP(t, fakeOpts{cert: cert, implicitTLS: true})
	m := NewSMTPMailer(SMTPConfig{Host: "example.com", Port: 465, From: "no-reply@example.com"}, zap.NewNop())
	m.dialAddr, m.rootCAs = addr, pool

	if err := sendVia(t, m); err != nil {
		t.Fatalf("send over implicit TLS: %v", err)
	}
	if s := <-got; !strings.HasPrefix(s, "tls:") {
		t.Errorf("want an encrypted session:\n%s", s)
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
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
