package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// Serve exposes GET /metrics on :port. Port 0 disables it. The port is bound
// before Serve returns, so a clash is reported to the caller; serving then
// continues in the background until ctx is done. The returned channel is
// closed once shutdown has finished (immediately closed when port is 0), so a
// caller can wait for it before exiting.
func Serve(ctx context.Context, port int, log *zap.Logger) (<-chan struct{}, error) {
	stopped := make(chan struct{})
	if port == 0 {
		close(stopped)
		return stopped, nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("metrics listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server", zap.Error(err))
		}
	}()
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Info("metrics listening", zap.Int("port", port))
	return stopped, nil
}
