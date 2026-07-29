#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=gate-lib.sh
source "${script_dir}/gate-lib.sh"

gate_enter_repo
gate_load_env
gate_require_command "test-mq-integration"
gate_require_env "test-mq-integration" \
	JXE_MYSQL_DSN \
	JXE_REDIS_ADDR \
	JXE_RABBITMQ_URL

mq_tests=(
	TestRabbitMQAcceptanceFailureAndRecovery
	TestCacheBackbonePublishConsumeAndDuplicate100
	TestConcurrentPublishersClaimDifferentEvents
	TestPublisherReclaimsExpiredLease
)
test_pattern="$(gate_exact_test_pattern "${mq_tests[@]}")"

JXE_RUN_INTEGRATION=1 \
	JXE_RUN_MQ_INTEGRATION=1 \
	"${gate_go}" test -p 1 -count=1 ./internal/modules/mq -run "${test_pattern}"
