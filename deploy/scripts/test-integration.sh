#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=gate-lib.sh
source "${script_dir}/gate-lib.sh"

gate_enter_repo
gate_load_env
gate_require_command "test-integration"
gate_require_env "test-integration" JXE_MYSQL_DSN

wine_ticket_purchase_enabled="${JXE_WINE_TICKET_PURCHASE_ENABLED:-true}"
asset_package="./internal/modules/asset"
app_package="./internal/app"
asset_tests=(
	TestL4LedgerAcceptanceIntegration
	TestL4AssetFormalAcceptanceGaps
)
app_tests=(
	TestDeliveryIncidentHTTPAndNaturalClosureEndToEnd
)
required_tests=(
	"${asset_tests[@]}"
	"${app_tests[@]}"
)
integration_packages=(
	./internal/pkg/idempotency
	./internal/modules/order
	./internal/modules/refund
	./internal/modules/aftersale
	./internal/modules/deliveryreturn
	./internal/modules/reconciliation
	./internal/modules/dispatch
	"${asset_package}"
	./internal/modules/wineticket/...
)

gate_assert_tests_listed "test-integration" "${asset_package}" "${asset_tests[@]}"
gate_assert_tests_listed "test-integration" "${app_package}" "${app_tests[@]}"

result_file="$(mktemp "${TMPDIR:-/tmp}/jxe-test-integration.XXXXXX")"
cleanup() {
	rm -f "${result_file}"
}
trap cleanup EXIT INT TERM

JXE_RUN_INTEGRATION=1 \
	JXE_WINE_TICKET_ENABLED=true \
	JXE_WINE_TICKET_PURCHASE_ENABLED="${wine_ticket_purchase_enabled}" \
	"${gate_go}" test -json -p 1 -count=1 \
	"${integration_packages[@]}" | tee "${result_file}"

JXE_RUN_INTEGRATION=1 \
	JXE_WINE_TICKET_ENABLED=true \
	JXE_WINE_TICKET_PURCHASE_ENABLED="${wine_ticket_purchase_enabled}" \
	"${gate_go}" test -json -p 1 -count=1 \
	"${app_package}" \
	-run "$(gate_exact_test_pattern "${app_tests[@]}")" | tee -a "${result_file}"

gate_assert_tests_passed "test-integration" "${result_file}" "${required_tests[@]}"
