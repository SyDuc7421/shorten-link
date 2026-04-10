.PHONY: run build test migrate lint docker-up

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
