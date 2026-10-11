package main

import (
	"net/http"
	"testing"
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
