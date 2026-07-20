.PHONY: run run-worker fmt-check openapi-check alerts-check mq-contract-check mq-vhost-provision mq-topology-apply mq-topology-verify test test-race test-integration test-mq-integration payment-refund-gate acceptance-phase15 verify verify-cp1 tidy seed migrate-up migrate-down migrate-check provision-runtime-user deps-up deps-down deps-mq-up

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

fmt-check:
	@test -z "$$(gofmt -l ./cmd ./internal)" || (gofmt -l ./cmd ./internal && exit 1)

openapi-check:
	go test ./internal/modules/docs ./internal/app -run 'TestOpenAPIAndSwaggerRoutes|TestOpenAPICoversRegisteredBusinessRoutes' -count=1

alerts-check:
	docker run --rm --entrypoint=/bin/promtool -v "$(CURDIR)/deploy/prometheus:/rules:ro" prom/prometheus:v3.5.0 check rules /rules/alerts.yml

mq-contract-check:
	go test ./internal/modules/mq -run 'TestEventEnvelope|TestEnvelopeIgnores|TestRegistry|TestAllLiteralOutboxEvents|TestTopology' -count=1

mq-vhost-provision:
	bash ./deploy/rabbitmq/provision-local-vhost.sh "$${JXE_MQ_VHOST:-jxe-events-v2}" "$${JXE_MQ_LOCAL_USER:-jxe}"

mq-topology-apply:
	$(load_env) go run ./cmd/mq-topology

mq-topology-verify:
	$(load_env) go run ./cmd/mq-topology -verify-only

test-race:
	go test -race ./...

test-integration:
	$(load_env) JXE_MYSQL_RUNTIME_DSN="$$JXE_MYSQL_DSN" JXE_MYSQL_DSN="$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" JXE_RUN_INTEGRATION=1 go test -p 1 ./internal/... -count=1 -v

payment-refund-gate:
	go test -count=1 ./...
	go test -race ./internal/infra/wechatpay ./internal/modules/order ./internal/modules/refund ./internal/modules/aftersale
	go vet ./...
	$(load_env) JXE_RUN_INTEGRATION=1 go test -p 1 -count=1 ./internal/modules/order ./internal/modules/refund ./internal/modules/aftersale

test-mq-integration:
	@set -e; vhost=jxe-rmq-integration; \
		docker exec jxe-p0-rabbitmq rabbitmqctl delete_vhost "$$vhost" >/dev/null 2>&1 || true; \
		docker exec jxe-p0-rabbitmq rabbitmqctl add_vhost "$$vhost"; \
		docker exec jxe-p0-rabbitmq rabbitmqctl set_permissions -p "$$vhost" jxe '.*' '.*' '.*'; \
		trap 'docker exec jxe-p0-rabbitmq rabbitmqctl delete_vhost "$$vhost" >/dev/null' EXIT; \
		$(load_env) base="$$JXE_RABBITMQ_URL"; export JXE_RABBITMQ_URL="$${base%/*}/$$vhost"; \
		JXE_RUN_MQ_INTEGRATION=1 go test -p 1 ./internal/modules/mq ./internal/app \
			-run 'TestRabbitMQAcceptanceContractAndOperations|TestCacheBackbonePublishConsumeAndDuplicate100|TestRabbitMQAcceptanceFailureAndRecovery|TestRabbitMQDomainAcceptanceIntegration|TestDeliveryIncidentOutboxRabbitMQNotificationEndToEnd' -count=1 -v

# Phase 1.5 release gate: 55 original business regressions plus all 24 MQ
# acceptance IDs. The MQ suite provisions and removes its own isolated vhost.
acceptance-phase15: verify-cp1 mq-topology-verify test-mq-integration

verify: fmt-check openapi-check mq-contract-check
	go vet ./...
	go test -race ./...

verify-cp1: verify migrate-check seed test-integration alerts-check

tidy:
	go mod tidy

migrate-up:
	$(load_env) go run github.com/pressly/goose/v3/cmd/goose@v3.26.0 -dir ./migrations mysql "$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" up

migrate-down:
	$(load_env) go run github.com/pressly/goose/v3/cmd/goose@v3.26.0 -dir ./migrations mysql "$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" down

migrate-check:
	$(MAKE) migrate-up
	$(MAKE) provision-runtime-user

provision-runtime-user:
	bash ./deploy/mysql/provision-local-runtime-user.sh

seed:
	$(load_env) JXE_MYSQL_DSN="$${JXE_MYSQL_MIGRATION_DSN:-$$JXE_MYSQL_DSN}" go run ./cmd/seed

deps-up:
	docker compose -f deploy/docker-compose.local.yml --profile mq up -d

deps-mq-up:
	docker compose -f deploy/docker-compose.local.yml --profile mq up -d rabbitmq

deps-down:
	docker compose -f deploy/docker-compose.local.yml down
