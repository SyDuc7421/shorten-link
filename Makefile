.PHONY: run build test migrate lint docker-up admin-grant admin-revoke seed-admin observe observe-down observe-check

run:
	set -a && . ./.env && set +a && go run ./cmd/server

build:
	go build -o bin/server ./cmd/server
	go build -o bin/worker ./cmd/worker

test:
	go test ./...

migrate:
	set -a && . ./.env && set +a && migrate -path deploy/migrations -database "mysql://$$MYSQL_USER:$$MYSQL_PASSWORD@tcp($$MYSQL_HOST:$$MYSQL_PORT)/$$MYSQL_DB" up

lint:
	golangci-lint run ./...

docker-up:
	docker compose up -d

keys:
	mkdir -p keys
	openssl genrsa -out keys/private.pem 2048
	openssl rsa -in keys/private.pem -pubout -out keys/public.pem

# make admin-grant EMAIL=you@example.com (the user must have registered first)
admin-grant:
	set -a && . ./.env && set +a && go run ./cmd/server admin grant "$(EMAIL)"

admin-revoke:
	set -a && . ./.env && set +a && go run ./cmd/server admin revoke "$(EMAIL)"

# Create the dev admin from ADMIN_EMAIL / ADMIN_PASSWORD in .env (idempotent:
# an existing account is only granted the role, its password is kept).
seed-admin:
	set -a && . ./.env && set +a && \
	if [ -z "$$ADMIN_EMAIL" ]; then echo "set ADMIN_EMAIL and ADMIN_PASSWORD in .env"; exit 2; fi && \
	go run ./cmd/server admin create "$$ADMIN_EMAIL"

# --- Observability stack (docker-compose.observability.yml) ---

OBSERVE_COMPOSE = docker compose -f docker-compose.yml -f docker-compose.observability.yml --profile app
OBS_DIR = $(CURDIR)/deploy/observability

# Build and start the app, its dependencies and the observability stack.
observe:
	$(OBSERVE_COMPOSE) up -d --build --wait
	@echo "Grafana:      http://localhost:3000 (admin / GRAFANA_ADMIN_PASSWORD, default admin)"
	@echo "Prometheus:   http://localhost:9090"
	@echo "Alertmanager: http://localhost:9093"

# Stop everything started by make observe. Volumes (metrics, logs, Grafana) are kept.
observe-down:
	$(OBSERVE_COMPOSE) down

# Validate the configs with the same image versions as docker-compose.observability.yml.
observe-check:
	docker run --rm -v $(OBS_DIR)/prometheus:/etc/prometheus:ro --entrypoint promtool prom/prometheus:v3.16.0 check config /etc/prometheus/prometheus.yml
	docker run --rm -v $(OBS_DIR)/prometheus:/etc/prometheus:ro --entrypoint promtool prom/prometheus:v3.16.0 check rules /etc/prometheus/rules/shortlink.yml
	docker run --rm -v $(OBS_DIR)/alertmanager:/etc/alertmanager:ro --entrypoint amtool prom/alertmanager:v0.35.0 check-config /etc/alertmanager/alertmanager.yml
	docker run --rm -v $(OBS_DIR)/alloy:/etc/alloy:ro grafana/alloy:v1.22.0 fmt --test /etc/alloy/config.alloy > /dev/null && echo "alloy fmt: OK"
