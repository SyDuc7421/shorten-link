package metrics_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/april/shorten-link/internal/metrics"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestServe_ZeroPortDisabled(t *testing.T) {
	if err := metrics.Serve(context.Background(), 0, zap.NewNop()); err != nil {
		t.Fatalf("Serve(0) = %v, want nil", err)
	}
}

func TestServe_ExposesMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := freePort(t)
	if err := metrics.Serve(ctx, port, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"shortlink_http_requests_in_flight", "go_goroutines"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestServe_PortInUseErrors(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	if err := metrics.Serve(context.Background(), port, zap.NewNop()); err == nil {
		t.Fatal("expected bind error")
	}
}

func TestServe_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	port := freePort(t)
	if err := metrics.Serve(ctx, port, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err != nil {
			return
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("still listening after cancel")
}
