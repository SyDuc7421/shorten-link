package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/jmoiron/sqlx"
)

// HealthChecker holds the dependency clients used for liveness probes.
type HealthChecker struct {
	db   *sqlx.DB
	rdb  *redis.Client
	amqp *amqp.Connection
}

// NewHealthChecker constructs a HealthChecker.
func NewHealthChecker(db *sqlx.DB, rdb *redis.Client, conn *amqp.Connection) *HealthChecker {
	return &HealthChecker{db: db, rdb: rdb, amqp: conn}
}

// Register mounts the health route onto mux.
func (h *HealthChecker) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.Health)
}

// Health handles GET /healthz.
// Returns 200 with all statuses when healthy; 503 if any dependency is down.
func (h *HealthChecker) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	status := map[string]string{
		"mysql":    "ok",
		"redis":    "ok",
		"rabbitmq": "ok",
	}
	healthy := true

	if err := h.db.PingContext(ctx); err != nil {
		status["mysql"] = "down"
		healthy = false
	}
	if err := h.rdb.Ping(ctx).Err(); err != nil {
		status["redis"] = "down"
		healthy = false
	}
	if h.amqp.IsClosed() {
		status["rabbitmq"] = "down"
		healthy = false
	}

	code := http.StatusOK
	if !healthy {
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(status) //nolint:errcheck
}
