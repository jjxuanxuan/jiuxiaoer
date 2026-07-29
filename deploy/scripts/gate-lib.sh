#!/usr/bin/env bash

# 仓库验收门禁共享辅助函数。调用方在加载本文件前必须自行启用严格 Shell 选项。

gate_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
gate_repo_root="$(cd "${gate_script_dir}/../.." && pwd)"
gate_go="${GO:-go}"

gate_enter_repo() {
	cd "${gate_repo_root}"
}

gate_load_env() {
	local env_file="${ENV_FILE:-.env.local}"
	if [[ "${env_file}" != /* ]]; then
		env_file="${gate_repo_root}/${env_file}"
	fi
	if [[ -f "${env_file}" ]]; then
		set -a
		# shellcheck disable=SC1090
		source "${env_file}"
		set +a
	fi
}

gate_require_command() {
	if ! command -v "${gate_go}" >/dev/null 2>&1; then
		echo "FAIL $1: Go command '${gate_go}' is not available" >&2
		exit 2
	fi
}

# 外部依赖验收门禁按失败关闭处理。
# ALLOW_EXTERNAL_SKIP=1 仅用于本地显式跳过，CI 中绝不能设置。
gate_require_env() {
	local gate_name="$1"
	shift
	local missing=()
	local variable_name
	for variable_name in "$@"; do
		if [[ -z "${!variable_name:-}" ]]; then
			missing+=("${variable_name}")
		fi
	done
	if ((${#missing[@]} == 0)); then
		return
	fi
	if [[ "${ALLOW_EXTERNAL_SKIP:-0}" == "1" && "${CI:-false}" != "true" ]]; then
		echo "SKIP ${gate_name}: missing ${missing[*]}"
		exit 0
	fi
	echo "FAIL ${gate_name}: missing ${missing[*]}" >&2
	exit 2
}

gate_exact_test_pattern() {
	local IFS="|"
	printf '^(%s)$' "$*"
}

gate_assert_tests_listed() {
	local gate_name="$1"
	local package_name="$2"
	shift 2
	local available_tests
	available_tests="$("${gate_go}" test "${package_name}" -list '^Test')"
	local test_name
	for test_name in "$@"; do
		if ! grep -Fqx "${test_name}" <<<"${available_tests}"; then
			echo "FAIL ${gate_name}: required test ${test_name} is missing from ${package_name}" >&2
			exit 2
		fi
	done
}

gate_assert_tests_passed() {
	local gate_name="$1"
	local result_file="$2"
	shift 2
	local test_name
	for test_name in "$@"; do
		if ! awk -v test_name="${test_name}" '
			index($0, "\"Action\":\"pass\"") &&
			index($0, "\"Test\":\"" test_name "\"") { found = 1 }
			END { exit found ? 0 : 1 }
		' "${result_file}"; then
			echo "FAIL ${gate_name}: required test ${test_name} did not report PASS" >&2
			exit 2
		fi
		echo "${gate_name}: verified PASS ${test_name}"
	done
}
