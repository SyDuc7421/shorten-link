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

	"github.com/redis/go-redis/v9"
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

	// Click events are best effort: the server starts and keeps serving
	// redirects while RabbitMQ is down, and the publisher reconnects on its own.
	pub := messaging.NewPublisher(cfg.DialRabbitMQ, log)
	defer pub.Close()

	// RSA keys for JWT
	privateKey, publicKey, err := cfg.LoadRSAKeys()
	if err != nil {
		log.Fatal("failed to load RSA keys", zap.Error(err))
	}

	// ID generator
	gen, err := idgen.NewGenerator(cfg.NodeID)
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
	healthHandler := handler.NewHealthChecker(db, rdb, pub)

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

	root := withRateLimits(mux, rdb)

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

	srv := newServer(cfg.Port, chain)

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

// withRateLimits applies per-IP limits to the abuse-prone routes. Login and
// register run bcrypt (~0.25s CPU each), so they are limited tightly.
func withRateLimits(mux http.Handler, rdb *redis.Client) http.Handler {
	redirectRL := middleware.RateLimit(rdb, "redirect", 60, time.Minute)(mux)
	createRL := middleware.RateLimit(rdb, "create", 10, time.Minute)(mux)
	loginRL := middleware.RateLimit(rdb, "login", 10, time.Minute)(mux)
	registerRL := middleware.RateLimit(rdb, "register", 5, time.Hour)(mux)
	refreshRL := middleware.RateLimit(rdb, "refresh", 30, time.Minute)(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path != "/healthz" && !isAPIPath(r.URL.Path):
			redirectRL.ServeHTTP(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/links":
			createRL.ServeHTTP(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			loginRL.ServeHTTP(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/register":
			registerRL.ServeHTTP(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/refresh":
			refreshRL.ServeHTTP(w, r)
		default:
			mux.ServeHTTP(w, r)
		}
	})
}

func newServer(port int, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second, // > TimeoutHandler's 10s so its 503 body is written
		IdleTimeout:       60 * time.Second,
	}
}

func isAPIPath(path string) bool {
	return len(path) >= 4 && path[:4] == "/api"
}
