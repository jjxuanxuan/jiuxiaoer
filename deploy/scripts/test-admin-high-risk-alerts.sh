#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=gate-lib.sh
source "${script_dir}/gate-lib.sh"

gate_name="test-admin-high-risk-alerts"
prometheus_image="${JXE_PROMETHEUS_TEST_IMAGE:-prom/prometheus:v3.5.0}"

gate_enter_repo

if command -v promtool >/dev/null 2>&1; then
	(
		cd ./deploy/prometheus
		promtool test rules admin-high-risk-alerts.test.yml
	)
	echo "${gate_name}: PASS (local promtool)"
	exit 0
fi

if ! command -v docker >/dev/null 2>&1 ||
	! docker info >/dev/null 2>&1; then
	if [[ "${ALLOW_EXTERNAL_SKIP:-0}" == "1" &&
		"${CI:-false}" != "true" ]]; then
		echo "SKIP ${gate_name}: promtool and Docker are unavailable"
		exit 0
	fi
	echo "FAIL ${gate_name}: promtool or a running Docker daemon is required" >&2
	exit 2
fi

docker run --rm \
	--entrypoint=/bin/promtool \
	--workdir=/work \
	--volume "${gate_repo_root}/deploy/prometheus:/work:ro" \
	"${prometheus_image}" \
	test rules admin-high-risk-alerts.test.yml
echo "${gate_name}: PASS (${prometheus_image})"
