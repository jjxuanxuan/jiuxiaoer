package main

import (
	"os"
	"strings"
	"testing"
)

func TestIntegrationScriptsRespectPurchaseProfile(t *testing.T) {
	t.Parallel()

	for _, script := range []string{
		"test-integration.sh",
		"test-wine-ticket-reconciliation.sh",
	} {
		content := readGateFile(t, script)
		if !strings.Contains(
			content,
			`wine_ticket_purchase_enabled="${JXE_WINE_TICKET_PURCHASE_ENABLED:-true}"`,
		) {
			t.Errorf("%s does not preserve an explicit caller purchase profile", script)
		}
		if !strings.Contains(
			content,
			`JXE_WINE_TICKET_PURCHASE_ENABLED="${wine_ticket_purchase_enabled}"`,
		) {
			t.Errorf("%s does not pass the selected purchase profile to go test", script)
		}
	}
}

func TestIntegrationGateIncludesIdempotencyPackage(t *testing.T) {
	t.Parallel()

	content := readGateFile(t, "test-integration.sh")
	if !strings.Contains(content, "./internal/pkg/idempotency") {
		t.Fatal("test-integration.sh must run the real MySQL idempotency contract tests")
	}
}

func TestIntegrationGateRequiresSingleOperatorMySQLAcceptance(t *testing.T) {
	t.Parallel()

	content := readGateFile(t, "test-integration.sh")
	for _, required := range []string{
		`asset_package="./internal/modules/asset"`,
		`app_package="./internal/app"`,
		`TestL4LedgerAcceptanceIntegration`,
		`TestL4AssetFormalAcceptanceGaps`,
		`TestDeliveryIncidentHTTPAndNaturalClosureEndToEnd`,
		`gate_assert_tests_listed "test-integration"`,
		`gate_assert_tests_passed "test-integration"`,
	} {
		if !strings.Contains(content, required) {
			t.Errorf(
				"test-integration must fail closed on single-operator acceptance %q",
				required,
			)
		}
	}
}

func TestMigrateCheckRequiresSingleOperatorDownPermissionBoundary(t *testing.T) {
	t.Parallel()

	script := readGateFile(t, "migrate-check.sh")
	start := strings.Index(script, `single_operator_down_facts="$(`)
	end := strings.Index(script, `if [[ "${single_operator_down_facts}"`)
	if start < 0 || end <= start {
		t.Fatal("migrate-check must expose a bounded single-operator Down assertion block")
	}
	downAssertions := script[start:end]
	for _, required := range []string{
		`p.code='delivery:force_complete'`,
		`r.code IN ('admin_manager','operation')`,
		`r.code='super_admin'`,
	} {
		if !strings.Contains(downAssertions, required) {
			t.Errorf("single-operator Down assertions must contain %q", required)
		}
	}
	if !strings.Contains(script[end:], `$'4\n10\n0\n1\n1\n1'`) {
		t.Fatal("single-operator Down gate must require revoked manager/operation mappings and one retained super-admin mapping")
	}
}

func TestWineTicketIntegrityGateTracksRenamedDomain(t *testing.T) {
	t.Parallel()

	content := readGateFile(t, "test-wine-ticket-reconciliation.sh")
	if !strings.Contains(
		content,
		`reconciliation_pattern="Integrity|Reconciliation|Bill"`,
	) {
		t.Fatal(
			"wine-ticket integrity tests must remain selected after the " +
				"domain rename",
		)
	}
}

func TestWorkflowPinsExpandAndContractPurchaseProfiles(t *testing.T) {
	t.Parallel()

	workflow := readGateFile(t, "../../.github/workflows/payment-refund-gate.yml")
	for _, stepName := range []string{
		"Expand-schema integration gate",
		"Reconciliation gate",
	} {
		step := workflowStep(t, workflow, stepName)
		if !strings.Contains(step, `JXE_WINE_TICKET_PURCHASE_ENABLED: "false"`) {
			t.Errorf("%s must run against the Expand schema profile", stepName)
		}
	}

	contract := workflowStep(t, workflow, "Contract-schema regression gate")
	if !strings.Contains(
		contract,
		`JXE_WINE_TICKET_PURCHASE_ENABLED=true make test-integration`,
	) {
		t.Error("Contract-schema regression gate must enable the money Contract profile")
	}
	if !strings.Contains(
		contract,
		`-allow-missing -dir ./migrations/manual`,
	) {
		t.Error(
			"manual Contract must explicitly allow its audited out-of-order migration",
		)
	}
}

func TestWorkflowUsesRestrictedRuntimeDSNForAssetImmutability(t *testing.T) {
	t.Parallel()

	workflow := readGateFile(t, "../../.github/workflows/payment-refund-gate.yml")
	for _, required := range []string{
		`JXE_MYSQL_RUNTIME_DSN: "jxe_gate_runtime:`,
		`JXE_CONTRACT_RUNTIME_DSN: "jxe_gate_runtime:`,
		`name: Seed Expand acceptance fixtures`,
		`name: Provision restricted Expand runtime user`,
		`JXE_MYSQL_DATABASE=jxe`,
		`bash ./deploy/mysql/provision-local-runtime-user.sh`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("workflow must provision a restricted Expand runtime: missing %q", required)
		}
	}

	contract := workflowStep(t, workflow, "Contract-schema regression gate")
	for _, required := range []string{
		`JXE_MYSQL_DSN="$JXE_CONTRACT_DSN" go run ./cmd/seed`,
		`JXE_MYSQL_DATABASE=jxe_contract`,
		`JXE_MYSQL_RUNTIME_DSN="$JXE_CONTRACT_RUNTIME_DSN"`,
		`bash ./deploy/mysql/provision-local-runtime-user.sh`,
	} {
		if !strings.Contains(contract, required) {
			t.Errorf("Contract gate must provision and use its restricted runtime: missing %q", required)
		}
	}
}

func TestWorkflowWiresEveryRequiredMakeGate(t *testing.T) {
	t.Parallel()

	workflow := readGateFile(t, "../../.github/workflows/payment-refund-gate.yml")
	for _, target := range []string{
		"test-wine-ticket-e2e",
		"test-race",
		"openapi-check",
		"alerts-check",
		"test-integration",
		"test-mq-integration",
		"test-wine-ticket-concurrency",
		"test-wine-ticket-reconciliation",
		"migrate-check",
	} {
		if !strings.Contains(workflow, "make "+target) {
			t.Errorf("workflow must wire required Make gate %q", target)
		}
	}
}

func TestContractGateUsesIndependentDatabase(t *testing.T) {
	t.Parallel()

	workflow := readGateFile(t, "../../.github/workflows/payment-refund-gate.yml")
	contract := workflowStep(t, workflow, "Contract-schema regression gate")
	for _, required := range []string{
		`JXE_CONTRACT_DSN: "root:integration-root-password@tcp(127.0.0.1:3306)/jxe_contract?`,
		`CREATE DATABASE jxe_contract`,
		`mysql "$JXE_CONTRACT_DSN" up`,
		`JXE_MYSQL_DSN="$JXE_CONTRACT_DSN"`,
	} {
		if !strings.Contains(contract, required) {
			t.Errorf(
				"Contract-schema regression gate must contain %q",
				required,
			)
		}
	}
	if strings.Contains(
		contract,
		`JXE_MYSQL_DSN="$JXE_MYSQL_DSN"`,
	) {
		t.Error("Contract-schema gate must not reuse the Expand database DSN")
	}
}

func TestConcurrencyGateRunsFundsClosureOnContractSchema(t *testing.T) {
	t.Parallel()

	script := readGateFile(t, "test-wine-ticket-concurrency.sh")
	for _, required := range []string{
		`gate_require_env "${gate_name}" JXE_MYSQL_DSN JXE_CONTRACT_DSN`,
		`JXE_MYSQL_DSN="${JXE_CONTRACT_DSN}"`,
		`"${refund_package}"`,
		`"${contract_test_pattern}"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf(
				"concurrency gate must contain Contract routing %q",
				required,
			)
		}
	}

	workflow := readGateFile(t, "../../.github/workflows/payment-refund-gate.yml")
	contractIndex := strings.Index(
		workflow,
		"- name: Contract-schema regression gate",
	)
	concurrencyIndex := strings.Index(
		workflow,
		"- name: P0 MySQL concurrency gate",
	)
	if contractIndex < 0 || concurrencyIndex < 0 ||
		concurrencyIndex <= contractIndex {
		t.Error(
			"P0 concurrency gate must run after its isolated Contract " +
				"database is prepared",
		)
	}
}

func TestConcurrencyGateIncludesSingleOperatorMySQLAcceptance(t *testing.T) {
	t.Parallel()

	script := readGateFile(t, "test-wine-ticket-concurrency.sh")
	for _, required := range []string{
		`catalog_package="./internal/modules/wineticket/catalog"`,
		`ops_package="./internal/modules/wineticket/ops"`,
		`TestMySQLPackageSameCodeConcurrentPublishHasSingleWinner`,
		`TestMySQLExceptionResolutionConcurrentDifferentKeysExecutesOnce`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf(
				"concurrency gate must include single-operator acceptance %q",
				required,
			)
		}
	}

	for _, testArray := range []string{
		`"${catalog_tests[@]}"`,
		`"${ops_tests[@]}"`,
	} {
		if count := strings.Count(script, testArray); count < 3 {
			t.Errorf(
				"concurrency gate must list, run, and verify %s; references=%d",
				testArray,
				count,
			)
		}
	}

	for _, packageVariable := range []string{
		`"${catalog_package}"`,
		`"${ops_package}"`,
	} {
		if count := strings.Count(script, packageVariable); count < 2 {
			t.Errorf(
				"concurrency gate must run and validate %s; references=%d",
				packageVariable,
				count,
			)
		}
	}
}

func readGateFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func workflowStep(t *testing.T, workflow, name string) string {
	t.Helper()
	marker := "      - name: " + name
	start := strings.Index(workflow, marker)
	if start < 0 {
		t.Fatalf("workflow step %q is missing", name)
	}
	remaining := workflow[start+len(marker):]
	if next := strings.Index(remaining, "\n      - name: "); next >= 0 {
		remaining = remaining[:next]
	}
	return marker + remaining
}
