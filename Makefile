.PHONY: run run-worker test test-race tidy seed migrate-up migrate-down migrate-check \
	wine-ticket-backfill deps-up deps-down deps-mq-up test-integration \
	test-mq-integration openapi-check alerts-check test-wine-ticket-e2e \
	test-wine-ticket-concurrency test-wine-ticket-reconciliation

ENV_FILE ?= .env.local
ALLOW_EXTERNAL_SKIP ?= 0

GO ?= go
WINE_TICKET_E2E_PATTERN := WineTicket|Package|Purchase|Cabinet|Redemption|Gift|Reminder|Renewal|Refund|SlotAdmin|Expiry

define load_env
set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a;
endef

run:
	$(load_env) go run ./cmd/api

run-worker:
	$(load_env) go run ./cmd/worker -role "$${JXE_WORKER_ROLE:-all}"

test:
	$(GO) test ./...

test-race:
	$(GO) run ./tools/check-architecture
	$(GO) test -race -count=1 ./...

tidy:
	go mod tidy

migrate-up:
	$(load_env) go run github.com/pressly/goose/v3/cmd/goose@v3.26.0 -dir ./migrations mysql "$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" up

migrate-down:
	$(load_env) go run github.com/pressly/goose/v3/cmd/goose@v3.26.0 -dir ./migrations mysql "$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" down

# 只使用一次性 MySQL 8.4 容器，绝不会指向 ENV_FILE。
# Docker 缺失时直接失败，除非显式设置 ALLOW_EXTERNAL_SKIP=1。
migrate-check:
	ALLOW_EXTERNAL_SKIP="$(ALLOW_EXTERNAL_SKIP)" bash ./deploy/scripts/migrate-check.sh

# 默认只做 dry-run。写入还需要 EXECUTE=--execute、专用环境门禁、
# CONFIRM=APPLY_WINE_TICKET_REGISTRY_BACKFILL 和私有 CHECKPOINT 路径。
wine-ticket-backfill:
	$(load_env) go run ./cmd/wine-ticket-backfill --job "$(JOB)" $(EXECUTE) --confirm "$(CONFIRM)" --checkpoint "$(CHECKPOINT)" --report "$(REPORT)"

seed:
	$(load_env) JXE_MYSQL_DSN="$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" go run ./cmd/seed

deps-up:
	docker compose -f deploy/docker-compose.local.yml --profile mq up -d

deps-mq-up:
	docker compose -f deploy/docker-compose.local.yml --profile mq up -d rabbitmq

deps-down:
	docker compose -f deploy/docker-compose.local.yml down

# 外部验收目标在依赖缺失时按失败关闭处理。
# 开发者可以通过 ALLOW_EXTERNAL_SKIP=1 显式跳过，
# CI 中绝不能设置该变量。
test-integration:
	@ENV_FILE="$(ENV_FILE)" ALLOW_EXTERNAL_SKIP="$(ALLOW_EXTERNAL_SKIP)" GO="$(GO)" \
		bash ./deploy/scripts/test-integration.sh

test-mq-integration:
	@ENV_FILE="$(ENV_FILE)" ALLOW_EXTERNAL_SKIP="$(ALLOW_EXTERNAL_SKIP)" GO="$(GO)" \
		bash ./deploy/scripts/test-mq-integration.sh

openapi-check:
	$(GO) test -count=1 ./internal/modules/docs
	$(GO) test -count=1 ./internal/app -run 'TestOpenAPICoversRegisteredBusinessRoutes|TestWineTicket.*Routes'

alerts-check:
	$(GO) run ./deploy/scripts/check-alerts.go ./deploy/prometheus/alerts.yml
	ALLOW_EXTERNAL_SKIP="$(ALLOW_EXTERNAL_SKIP)" bash ./deploy/scripts/test-admin-high-risk-alerts.sh
	$(GO) test -count=1 ./internal/modules/mq -run TestRabbitMQAcceptanceContractAndOperations

test-wine-ticket-e2e:
	$(GO) test -count=1 ./internal/modules/wineticket/...
	$(GO) test -count=1 ./internal/app -run '$(WINE_TICKET_E2E_PATTERN)'
	$(GO) test -count=1 ./internal/modules/mq -run WineTicket

test-wine-ticket-concurrency:
	@ENV_FILE="$(ENV_FILE)" ALLOW_EXTERNAL_SKIP="$(ALLOW_EXTERNAL_SKIP)" GO="$(GO)" \
		bash ./deploy/scripts/test-wine-ticket-concurrency.sh

test-wine-ticket-reconciliation:
	@ENV_FILE="$(ENV_FILE)" ALLOW_EXTERNAL_SKIP="$(ALLOW_EXTERNAL_SKIP)" GO="$(GO)" \
		bash ./deploy/scripts/test-wine-ticket-reconciliation.sh
