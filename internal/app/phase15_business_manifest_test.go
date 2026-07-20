package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type phase15BusinessCase struct {
	ID       string
	Modes    string
	File     string
	TestName string
}

// phase15BusinessCases is the executable traceability manifest for the 55
// original Phase 1 business acceptance IDs. The referenced tests are executed
// by make verify-cp1 before the MQ-specific gate runs.
var phase15BusinessCases = []phase15BusinessCase{
	{"ACC-C-001", "N", "internal/app/p0_integration_test.go", "TestP0Integration"},
	{"ACC-C-002", "N,F", "internal/app/l2_catalog_cart_acceptance_test.go", "TestL2CatalogAndCartMissingAcceptanceScenarios"},
	{"ACC-C-003", "N,F", "internal/app/l2_order_acceptance_test.go", "TestL2OrderMissingAcceptanceScenarios"},
	{"ACC-C-004", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-C-005", "N,F,D", "internal/app/p0_integration_test.go", "TestP0ConcurrentOrdersDoNotOversell"},
	{"ACC-C-006", "N", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-C-007", "N,F,D,R", "internal/app/p0_integration_test.go", "TestP0Integration"},
	{"ACC-C-008", "N,F,D,R", "internal/modules/order/payment_expiry_race_integration_test.go", "TestPaymentCallbackAndExpiryRace1000"},

	{"ACC-PRINT-001", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-PRINT-002", "N,F,D,R", "internal/app/mq_domain_acceptance_integration_test.go", "TestRabbitMQDomainAcceptanceIntegration"},
	{"ACC-PRINT-003", "N,F,R", "internal/modules/printjob/service_test.go", "TestPrintBackoffIsBounded"},
	{"ACC-PRINT-004", "N,F,D", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-PRINT-005", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-PRINT-006", "N,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-STOCK-001", "N,F,D,R", "internal/app/l2_home_cache_sre_acceptance_test.go", "TestL2HomeCacheAndSREMissingAcceptanceScenarios"},
	{"ACC-STOCK-002", "N,F,D,R", "internal/app/p0_integration_test.go", "TestP0ConcurrentOrdersDoNotOversell"},

	{"ACC-NOTIFY-001", "N,F,D,R", "internal/app/mq_domain_acceptance_integration_test.go", "TestRabbitMQDomainAcceptanceIntegration"},
	{"ACC-NOTIFY-002", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-NOTIFY-003", "N,F,D,R", "internal/modules/notification/service_test.go", "TestNotificationProviders"},
	{"ACC-NOTIFY-004", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-NOTIFY-005", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},

	{"ACC-VERIFY-001", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-VERIFY-002", "N", "internal/modules/deliveryverification/service_test.go", "TestVerificationDTOAttemptBudget"},
	{"ACC-VERIFY-003", "N", "internal/modules/deliveryverification/service_test.go", "TestVerificationDTOAttemptBudget"},
	{"ACC-VERIFY-004", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-VERIFY-005", "N", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-VERIFY-006", "N", "internal/modules/deliveryverification/service_test.go", "TestGeneratedCodesAndDomainSeparatedHashes"},
	{"ACC-VERIFY-007", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-VERIFY-008", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},

	{"ACC-PROV-001", "N,F,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-PROV-002", "N,F,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-PROV-003", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-PROV-004", "N,F,R", "internal/modules/provisioning/service_test.go", "TestRandomSecretAndAdminPermission"},
	{"ACC-PROV-005", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-PROV-006", "N", "internal/modules/provisioning/service_test.go", "TestRandomSecretAndAdminPermission"},
	{"ACC-OPS-001", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-OPS-002", "N,F,D,R", "internal/modules/refund/acceptance_integration_test.go", "TestL3RefundAcceptance"},
	{"ACC-OPS-003", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-OPS-004", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-OPS-005", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-OPS-006", "N,F,D,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-OPS-007", "N,F,R", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-OPS-008", "N", "internal/modules/admin/service_test.go", "TestAuditLogDTOIncludesBeforeAfter"},

	{"ACC-COMP-001", "N", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-COMP-002", "N,F", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-COMP-003", "N,F", "internal/app/cp1_acceptance_integration_test.go", "TestCP1ClosureAcceptanceIntegration"},
	{"ACC-COMP-004", "N", "internal/modules/compliance/provider_test.go", "TestFakeProviderRejectsInvalidCallback"},
	{"ACC-COMP-005", "N,D", "internal/modules/mq/acceptance_test.go", "TestRabbitMQAcceptanceContractAndOperations"},
	{"ACC-COMP-006", "N", "internal/modules/compliance/provider_test.go", "TestProviderAdultResultContract"},

	{"ACC-SRE-001", "N", "internal/infra/mysql/mysql_test.go", "TestValidateRequiredSchemaColumnsAcceptsMigratedSchema"},
	{"ACC-SRE-002", "N", "internal/infra/mysql/mysql_test.go", "TestValidateRequiredSchemaColumnsAcceptsMigratedSchema"},
	{"ACC-SRE-003", "N,R", "internal/infra/mysql/mysql_test.go", "TestValidateRequiredSchemaColumnsRejectsDriftedSchema"},
	{"ACC-SRE-004", "N,F,R", "internal/modules/health/handler_test.go", "TestOptionalDependenciesDoNotBlockReadiness"},
	{"ACC-SRE-005", "N,R", "internal/app/router_contract_test.go", "TestProductionRouterDoesNotRegisterMockRoutes"},
	{"ACC-SRE-006", "N,R", "internal/config/config_test.go", "TestProductionConfigAcceptsExplicitSafeValues"},
}

// TestPhase15BusinessRegressionCoverageManifest 验证Phase 15 Business Regression Coverage 清单的预期行为。
func TestPhase15BusinessRegressionCoverageManifest(t *testing.T) {
	if len(phase15BusinessCases) != 55 {
		t.Fatalf("business acceptance manifest has %d cases, want 55", len(phase15BusinessCases))
	}
	seen := make(map[string]bool, len(phase15BusinessCases))
	for _, acceptance := range phase15BusinessCases {
		acceptance := acceptance
		t.Run(acceptance.ID, func(t *testing.T) {
			if seen[acceptance.ID] {
				t.Fatalf("duplicate acceptance ID %s", acceptance.ID)
			}
			seen[acceptance.ID] = true
			if !strings.HasPrefix(acceptance.Modes, "N") {
				t.Fatalf("%s must include the RabbitMQ normal path N", acceptance.ID)
			}
			path := filepath.Join("..", "..", filepath.FromSlash(acceptance.File))
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read evidence file %s: %v", acceptance.File, err)
			}
			if !bytes.Contains(content, []byte("func "+acceptance.TestName+"(")) {
				t.Fatalf("evidence test %s is missing from %s", acceptance.TestName, acceptance.File)
			}
		})
	}
}
