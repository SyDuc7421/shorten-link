package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/april/shorten-link/config"
	"github.com/april/shorten-link/internal/logger"
	"github.com/april/shorten-link/internal/middleware"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	log, err := logger.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	ctx := context.Background()

	db, err := cfg.OpenMySQL(ctx)
	if err != nil {
		log.Fatal("failed to connect MySQL", zap.Error(err))
	}
	defer db.Close()
	log.Info("MySQL connected")

	rdb, err := cfg.NewRedisClient(ctx)
	if err != nil {
		log.Fatal("failed to connect Redis", zap.Error(err))
	}
	defer rdb.Close()
	log.Info("Redis connected")

	mqConn, err := cfg.DialRabbitMQ()
	if err != nil {
		log.Fatal("failed to connect RabbitMQ", zap.Error(err))
	}
	defer mqConn.Close()
	log.Info("RabbitMQ connected")

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: middleware.RequestID(log)(mux),
	}

	go func() {
		log.Info("server starting", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("forced shutdown", zap.Error(err))
	}
	log.Info("stopped")
}
