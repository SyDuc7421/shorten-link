.PHONY: run build test migrate lint docker-up

run:
	set -a && . ./.env && set +a && go run ./cmd/server

build:
	go build -o bin/server ./cmd/server
	go build -o bin/worker ./cmd/worker

test:
	go test ./...

migrate:
	migrate -path deploy/migrations -database "$$MYSQL_DSN" up

lint:
	golangci-lint run ./...

docker-up:
	docker compose up -d
