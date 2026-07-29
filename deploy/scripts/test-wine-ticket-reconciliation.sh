#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=gate-lib.sh
source "${script_dir}/gate-lib.sh"

gate_name="test-wine-ticket-reconciliation"
reconciliation_pattern="Integrity|Reconciliation|Bill"

gate_enter_repo
gate_load_env
gate_require_command "${gate_name}"
gate_require_env "${gate_name}" JXE_MYSQL_DSN

wine_ticket_purchase_enabled="${JXE_WINE_TICKET_PURCHASE_ENABLED:-true}"
JXE_RUN_INTEGRATION=1 \
	JXE_WINE_TICKET_ENABLED=true \
	JXE_WINE_TICKET_PURCHASE_ENABLED="${wine_ticket_purchase_enabled}" \
	"${gate_go}" test -p 1 -count=1 \
	./internal/modules/wineticket/... \
	./internal/modules/reconciliation \
	-run "${reconciliation_pattern}"
