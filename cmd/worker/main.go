package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"github.com/april/shorten-link/config"
	"github.com/april/shorten-link/internal/logger"
	"github.com/april/shorten-link/internal/messaging"
	"github.com/april/shorten-link/internal/repository"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := cfg.OpenMySQL(ctx)
	if err != nil {
		log.Fatal("failed to connect MySQL", zap.Error(err))
	}
	defer db.Close()
	log.Info("MySQL connected")

	mqConn, err := cfg.DialRabbitMQ()
	if err != nil {
		log.Fatal("failed to connect RabbitMQ", zap.Error(err))
	}
	defer mqConn.Close()
	log.Info("RabbitMQ connected")

	if err := messaging.Setup(mqConn); err != nil {
		log.Fatal("failed to setup messaging", zap.Error(err))
	}

	repo := repository.NewLinkMySQL(db)

	// click consumer channel
	clickCh, err := mqConn.Channel()
	if err != nil {
		log.Fatal("failed to open click consumer channel", zap.Error(err))
	}
	defer clickCh.Close()

	// DLQ consumer channel
	dlqCh, err := mqConn.Channel()
	if err != nil {
		log.Fatal("failed to open DLQ consumer channel", zap.Error(err))
	}
	defer dlqCh.Close()

	clickConsumer := messaging.NewClickConsumer(clickCh, repo, log)
	dlqConsumer := messaging.NewDLQConsumer(dlqCh, log)

	errc := make(chan error, 2)

	go func() {
		log.Info("click consumer started")
		errc <- clickConsumer.Start(ctx)
	}()

	go func() {
		log.Info("DLQ consumer started")
		errc <- dlqConsumer.Start(ctx)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Info("signal received, shutting down", zap.String("signal", sig.String()))
		cancel()
	case err := <-errc:
		log.Error("consumer error", zap.Error(err))
		cancel()
	}

	// drain both consumers
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			log.Error("consumer shutdown error", zap.Error(err))
		}
	}

	log.Info("worker stopped")
}
