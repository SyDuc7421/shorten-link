package main

import (
	"net/http"
	"net/url"
	"testing"

	"go.uber.org/zap"

	"github.com/april/shorten-link/config"
	"github.com/april/shorten-link/internal/mail"
)

func TestNewServer_HasTimeouts(t *testing.T) {
	srv := newServer(8080, http.NotFoundHandler())
	if srv.Addr != ":8080" {
		t.Errorf("addr: got %q", srv.Addr)
	}
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Errorf("missing timeouts: %+v", srv)
	}
}

func TestNewMailer(t *testing.T) {
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	log := zap.NewNop()

	// No SMTP and a local GUI: log the links, for development.
	for _, local := range []string{"http://localhost:3000/reset-password", "http://127.0.0.1:3000/r", "http://[::1]:3000/r"} {
		m, err := newMailer(&config.Config{}, parse(local), log)
		if err != nil {
			t.Errorf("%s: unexpected error %v", local, err)
		}
		if _, ok := m.(*mail.LogMailer); !ok {
			t.Errorf("%s: want *mail.LogMailer, got %T", local, m)
		}
	}

	// No SMTP but a real GUI: refusing to start beats writing working reset
	// links to production logs.
	if _, err := newMailer(&config.Config{}, parse("https://app.example.com/reset-password"), log); err == nil {
		t.Error("public reset URL without SMTP_HOST: want error")
	}

	m, err := newMailer(&config.Config{SMTPHost: "smtp.example.com", SMTPPort: 587}, parse("https://app.example.com/r"), log)
	if err != nil {
		t.Fatalf("with SMTP: %v", err)
	}
	if _, ok := m.(*mail.SMTPMailer); !ok {
		t.Errorf("with SMTP: want *mail.SMTPMailer, got %T", m)
	}
}
