package service

import (
	"context"
	"testing"
	"time"

	"github.com/april/shorten-link/internal/ctxkey"
)

type ctxCapturePub struct{ ctx chan context.Context }

func (p ctxCapturePub) Publish(ctx context.Context, _ string, _ any) error {
	p.ctx <- ctx
	return nil
}

func TestPublishAsync_CarriesRequestID(t *testing.T) {
	pub := ctxCapturePub{ctx: make(chan context.Context, 1)}
	publishAsync(ctxkey.SetRequestID(context.Background(), "rid-1"), pub, "k", nil)
	select {
	case c := <-pub.ctx:
		if id, ok := ctxkey.RequestID(c); !ok || id != "rid-1" {
			t.Errorf("want rid-1, got %q ok=%v", id, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("not published")
	}
}

func TestLinkServicePublishAsync_CarriesRequestID(t *testing.T) {
	pub := ctxCapturePub{ctx: make(chan context.Context, 1)}
	s := &LinkService{pub: pub}
	s.publishAsync(ctxkey.SetRequestID(context.Background(), "rid-1"), "k", nil)
	select {
	case c := <-pub.ctx:
		if id, ok := ctxkey.RequestID(c); !ok || id != "rid-1" {
			t.Errorf("want rid-1, got %q ok=%v", id, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("not published")
	}
}
