package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
)

// startConsumers simulates n consumers that return when ctx is cancelled.
func startConsumers(ctx context.Context, errc chan<- error, n int) {
	for i := 0; i < n; i++ {
		go func() {
			<-ctx.Done()
			errc <- nil
		}()
	}
}

func TestWaitForShutdown_ConsumerExitsEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 2)

	startConsumers(ctx, errc, 1)
	errc <- errors.New("channel closed") // second consumer dies on its own

	done := make(chan bool)
	go func() {
		done <- waitForShutdown(zap.NewNop(), make(chan os.Signal), errc, cancel, 2)
	}()

	select {
	case failed := <-done:
		if !failed {
			t.Error("want failed=true when a consumer exits on its own")
		}
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown hung after a consumer exited")
	}
}

func TestWaitForShutdown_Signal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 2)
	startConsumers(ctx, errc, 2)

	quit := make(chan os.Signal, 1)
	quit <- os.Interrupt

	done := make(chan bool)
	go func() {
		done <- waitForShutdown(zap.NewNop(), quit, errc, cancel, 2)
	}()

	select {
	case failed := <-done:
		if failed {
			t.Error("want failed=false on signal shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown hung after signal")
	}
}
