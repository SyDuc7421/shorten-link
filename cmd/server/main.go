package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/april/shorten-link/config"
	"github.com/april/shorten-link/internal/handler"
	"github.com/april/shorten-link/internal/logger"
	"github.com/april/shorten-link/internal/messaging"
	"github.com/april/shorten-link/internal/middleware"
	"github.com/april/shorten-link/internal/repository"
	"github.com/april/shorten-link/internal/service"
	"github.com/april/shorten-link/pkg/idgen"
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

	if err := messaging.Setup(mqConn); err != nil {
		log.Fatal("failed to setup messaging", zap.Error(err))
	}

	pub, err := messaging.NewPublisher(mqConn, log)
	if err != nil {
		log.Fatal("failed to create publisher", zap.Error(err))
	}
	defer pub.Close()

	// RSA keys for JWT
	privateKey, publicKey, err := cfg.LoadRSAKeys()
	if err != nil {
		log.Fatal("failed to load RSA keys", zap.Error(err))
	}

	// ID generator
	gen, err := idgen.NewGenerator(1) // nodeID 1; use a per-pod config field in k8s
	if err != nil {
		log.Fatal("failed to create ID generator", zap.Error(err))
	}

	// repositories
	linkRepo := repository.NewLinkMySQL(db)
	linkCache := repository.NewLinkRedis(rdb)
	userRepo := repository.NewUserMySQL(db)
	tokenCache := repository.NewTokenRedis(rdb)

	// services
	linkSvc := service.NewLinkService(linkRepo, linkCache, gen, pub)
	authSvc := service.NewAuthService(userRepo, tokenCache, gen, privateKey, publicKey)

	// handlers
	linkHandler := handler.NewLinkHandler(linkSvc, cfg.BaseURL)
	authHandler := handler.NewAuthHandler(authSvc)
	healthHandler := handler.NewHealthChecker(db, rdb, mqConn)

	// CORS origins from config
	var allowedOrigins []string
	if cfg.AllowedOrigins != "" {
		for _, o := range strings.Split(cfg.AllowedOrigins, ",") {
			if s := strings.TrimSpace(o); s != "" {
				allowedOrigins = append(allowedOrigins, s)
			}
		}
	}

	mux := http.NewServeMux()
	healthHandler.Register(mux)
	authHandler.Register(mux)
	linkHandler.Register(mux, middleware.RequireAuth)
	handler.RegisterDocs(mux)

	// rate limiting
	redirectRL := middleware.RateLimit(rdb, "redirect", 60, time.Minute)
	createRL := middleware.RateLimit(rdb, "create", 10, time.Minute)

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path != "/healthz" && !isAPIPath(r.URL.Path):
			redirectRL(mux).ServeHTTP(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/links":
			createRL(mux).ServeHTTP(w, r)
		default:
			mux.ServeHTTP(w, r)
		}
	})

	// Middleware chain (outermost → innermost):
	// TimeoutHandler → SecureHeaders → CORS → JWTAuth → RequestID → root
	chain := http.TimeoutHandler(
		middleware.SecureHeaders(
			middleware.CORS(allowedOrigins)(
				middleware.JWTAuth(authSvc)(
					middleware.RequestID(log)(
						middleware.MaxBodySize(root),
					),
				),
			),
		),
		10*time.Second,
		`{"error":"request timeout"}`,
	)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: chain,
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

func isAPIPath(path string) bool {
	return len(path) >= 4 && path[:4] == "/api"
}
