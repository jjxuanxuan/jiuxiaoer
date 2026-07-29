#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=gate-lib.sh
source "${script_dir}/gate-lib.sh"

gate_name="test-wine-ticket-concurrency"
purchase_package="./internal/modules/wineticket/purchase"
gift_package="./internal/modules/wineticket/gift"
redemption_package="./internal/modules/wineticket/redemption"
refund_package="./internal/modules/wineticket/refund"
reminder_package="./internal/modules/wineticket/reminder"
catalog_package="./internal/modules/wineticket/catalog"
ops_package="./internal/modules/wineticket/ops"

purchase_tests=(
	TestMySQLPurchaseSettlementConcurrentFailureAndSuccessIssuesExactlyOnce
	TestMySQLLatePurchaseSettlementFailureNeverDowngradesFundsTerminalState
	TestMySQLIssuanceCompensationRejectsPartialEntitlementFacts
)
gift_tests=(
	TestMySQLGiftClaimConcurrent100ExactlyOneSuccess
)
redemption_tests=(
	TestMySQLRedemptionConcurrent100LastBottleNoOverRedemption
	TestMySQLRedemptionConcurrent100HonorsExactSlotCapacity
)
refund_tests=(
	TestMySQLRefundConcurrentDuplicateSuccessClosesFundsAndEntitlementOnce
)
reminder_tests=(
	TestMySQLReminderLeaseAllowsExactlyOneConcurrentSender
)
catalog_tests=(
	TestMySQLPackageSameCodeConcurrentPublishHasSingleWinner
)
ops_tests=(
	TestMySQLExceptionResolutionConcurrentDifferentKeysExecutesOnce
)
required_tests=(
	"${purchase_tests[@]}"
	"${gift_tests[@]}"
	"${redemption_tests[@]}"
	"${refund_tests[@]}"
	"${reminder_tests[@]}"
	"${catalog_tests[@]}"
	"${ops_tests[@]}"
)
packages=(
	"${purchase_package}"
	"${gift_package}"
	"${redemption_package}"
	"${reminder_package}"
	"${catalog_package}"
	"${ops_package}"
)

gate_enter_repo
gate_load_env
gate_require_command "${gate_name}"
gate_require_env "${gate_name}" JXE_MYSQL_DSN JXE_CONTRACT_DSN
gate_assert_tests_listed \
	"${gate_name}" "${purchase_package}" "${purchase_tests[@]}"
gate_assert_tests_listed \
	"${gate_name}" "${gift_package}" "${gift_tests[@]}"
gate_assert_tests_listed \
	"${gate_name}" "${redemption_package}" "${redemption_tests[@]}"
gate_assert_tests_listed \
	"${gate_name}" "${refund_package}" "${refund_tests[@]}"
gate_assert_tests_listed \
	"${gate_name}" "${reminder_package}" "${reminder_tests[@]}"
gate_assert_tests_listed \
	"${gate_name}" "${catalog_package}" "${catalog_tests[@]}"
gate_assert_tests_listed \
	"${gate_name}" "${ops_package}" "${ops_tests[@]}"

expand_test_pattern="$(
	gate_exact_test_pattern \
		"${purchase_tests[@]}" \
		"${gift_tests[@]}" \
		"${redemption_tests[@]}" \
		"${reminder_tests[@]}" \
		"${catalog_tests[@]}" \
		"${ops_tests[@]}"
)"
contract_test_pattern="$(gate_exact_test_pattern "${refund_tests[@]}")"
result_file="$(mktemp "${TMPDIR:-/tmp}/jxe-wine-ticket-concurrency.XXXXXX")"
cleanup() {
	rm -f "${result_file}"
}
trap cleanup EXIT INT TERM

JXE_RUN_INTEGRATION=1 \
	JXE_WINE_TICKET_ENABLED=true \
	JXE_WINE_TICKET_PURCHASE_ENABLED=true \
	"${gate_go}" test -json -p 1 -count=1 \
	"${packages[@]}" \
	-run "${expand_test_pattern}" | tee "${result_file}"

JXE_MYSQL_DSN="${JXE_CONTRACT_DSN}" \
	JXE_RUN_INTEGRATION=1 \
	JXE_WINE_TICKET_ENABLED=true \
	JXE_WINE_TICKET_PURCHASE_ENABLED=true \
	"${gate_go}" test -json -p 1 -count=1 \
	"${refund_package}" \
	-run "${contract_test_pattern}" | tee -a "${result_file}"

gate_assert_tests_passed "${gate_name}" "${result_file}" "${required_tests[@]}"
echo "${gate_name}: PASS (${#required_tests[@]} required P0 MySQL tests)"
