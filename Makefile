.PHONY: run run-worker test tidy seed migrate-up migrate-down deps-up deps-down deps-mq-up

ENV_FILE ?= .env.local

define load_env
set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a;
endef

run:
	$(load_env) go run ./cmd/api

run-worker:
	$(load_env) go run ./cmd/worker -role "$${JXE_WORKER_ROLE:-all}"

test:
	go test ./...

tidy:
	go mod tidy

migrate-up:
	$(load_env) go run github.com/pressly/goose/v3/cmd/goose@v3.26.0 -dir ./migrations mysql "$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" up

migrate-down:
	$(load_env) go run github.com/pressly/goose/v3/cmd/goose@v3.26.0 -dir ./migrations mysql "$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" down

seed:
	$(load_env) JXE_MYSQL_DSN="$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" go run ./cmd/seed

deps-up:
	docker compose -f deploy/docker-compose.local.yml --profile mq up -d

deps-mq-up:
	docker compose -f deploy/docker-compose.local.yml --profile mq up -d rabbitmq

deps-down:
	docker compose -f deploy/docker-compose.local.yml down
