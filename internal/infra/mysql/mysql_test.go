package mysql

import (
	"os"
	"strings"
	"testing"
)

// TestValidateRequiredSchemaColumnsRejectsDriftedSchema 验证必需列检查拒绝发生偏移的表结构。
func TestValidateRequiredSchemaColumnsRejectsDriftedSchema(t *testing.T) {
	found := make(map[schemaColumn]bool, len(requiredSchemaColumns))
	for _, column := range requiredSchemaColumns {
		found[column] = true
	}
	delete(found, schemaColumn{table: "products", column: "category_id"})

	err := validateRequiredSchemaColumns(found)
	if err == nil {
		t.Fatal("expected schema validation error")
	}
	if !strings.Contains(err.Error(), "missing products.category_id") || !strings.Contains(err.Error(), "do not share") {
		t.Fatalf("expected actionable schema error, got %q", err)
	}
}

// TestValidateRequiredSchemaColumnsAcceptsMigratedSchema 验证必需列检查接受已迁移的表结构。
func TestValidateRequiredSchemaColumnsAcceptsMigratedSchema(t *testing.T) {
	found := make(map[schemaColumn]bool, len(requiredSchemaColumns))
	for _, column := range requiredSchemaColumns {
		found[column] = true
	}
	if err := validateRequiredSchemaColumns(found); err != nil {
		t.Fatalf("expected migrated schema to pass, got %v", err)
	}
}

func TestSearchDiscoveryTablesAreStartupSchemaRequirements(t *testing.T) {
	found := make(map[schemaColumn]bool, len(requiredSchemaColumns))
	for _, column := range requiredSchemaColumns {
		found[column] = true
	}
	delete(found, schemaColumn{table: "customer_search_histories", column: "normalized_keyword"})
	err := validateRequiredSchemaColumns(found)
	if err == nil || !strings.Contains(err.Error(), "missing customer_search_histories.normalized_keyword") {
		t.Fatalf("expected missing search history schema to fail startup verification, got %v", err)
	}
}

func TestWineTicketSchemaProfileFailsClosed(t *testing.T) {
	found := make(map[schemaColumn]bool, len(requiredSchemaColumns)+len(requiredWineTicketSchemaColumns))
	for _, column := range requiredSchemaColumns {
		found[column] = true
	}
	for _, column := range requiredWineTicketSchemaColumns {
		found[column] = true
	}
	delete(found, schemaColumn{table: "wine_ticket_lots", column: "lot_no"})

	err := validateRequiredSchemaColumnsForProfile(found, true)
	if err == nil || !strings.Contains(err.Error(), "missing wine_ticket_lots.lot_no") {
		t.Fatalf("expected wine-ticket schema profile to fail closed, got %v", err)
	}
}

func TestWineTicketSchemaProfileUsesRealAllocationColumns(t *testing.T) {
	required := make(map[schemaColumn]bool, len(requiredWineTicketSchemaColumns))
	for _, column := range requiredWineTicketSchemaColumns {
		required[column] = true
	}
	for _, phantom := range []schemaColumn{
		{table: "wine_ticket_redemption_allocations", column: "action_key"},
		{table: "wine_ticket_gift_allocations", column: "action_key"},
		{table: "wine_ticket_refund_allocations", column: "action_key"},
	} {
		if required[phantom] {
			t.Fatalf("readiness still requires nonexistent column: %+v", phantom)
		}
	}
	for _, actual := range []schemaColumn{
		{table: "wine_ticket_redemption_allocations", column: "redemption_id"},
		{table: "wine_ticket_gift_allocations", column: "gift_id"},
		{table: "wine_ticket_refund_allocations", column: "wine_ticket_refund_id"},
	} {
		if !required[actual] {
			t.Fatalf("readiness no longer verifies allocation table: %+v", actual)
		}
	}
}

func TestWineTicketExpandMigrationHasStableGiftAndRefundIndexes(t *testing.T) {
	body, err := os.ReadFile("../../../migrations/202607270001_wine_ticket_expand.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	if count := strings.Count(
		sql,
		"KEY idx_wt_gift_receiver_created (receiver_customer_id, created_at, id)",
	); count != 1 {
		t.Fatalf("gift receiver index declarations=%d want=1", count)
	}
	if !strings.Contains(sql, "KEY idx_wt_refund_purchase (purchase_id, id)") {
		t.Fatal("wine ticket refund purchase projection index is missing")
	}
}

func TestWineTicketRBACMigrationSeparatesPackagePublishAndUnpublish(t *testing.T) {
	body, err := os.ReadFile("../../../migrations/202607270002_wine_ticket_rbac.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	for _, declaration := range []string{
		"(2164, 'wine_ticket_package:publish', 'wine_ticket_package', 'publish', '发布酒票套餐', 'active')",
		"(2186, 'wine_ticket_package:unpublish', 'wine_ticket_package', 'unpublish', '下架酒票套餐', 'active')",
		"p.id BETWEEN 2146 AND 2186",
		"permission_id BETWEEN 2146 AND 2186",
	} {
		if !strings.Contains(sql, declaration) {
			t.Fatalf("wine-ticket RBAC migration is missing %q", declaration)
		}
	}
	if strings.Contains(sql, "发布或下架酒票套餐") {
		t.Fatal("wine-ticket RBAC migration still aliases unpublish to publish")
	}
}

func TestSingleOperatorMigrationRetiresDualReviewPermissions(t *testing.T) {
	body, err := os.ReadFile("../../../migrations/202607290001_single_operator_admin_actions.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	for _, declaration := range []string{
		"(2048, 'asset_adjustment:create'",
		"(2068, 'delivery:force_complete'",
		"(2148, 'wine_ticket_exception:resolve'",
		"(2164, 'wine_ticket_package:publish'",
		"SET status = 'inactive'",
		"'asset_adjustment:approve'",
		"'delivery:force_complete_request'",
		"'delivery:force_complete_approve'",
		"'wine_ticket_exception:review'",
		"p.id = 2068 AND p.code = 'delivery:force_complete' AND r.code IN ('super_admin', 'admin_manager', 'operation')",
		"WHERE action = 'delivery.force_complete' AND status = 'pending'",
	} {
		if !strings.Contains(sql, declaration) {
			t.Fatalf("single-operator migration is missing %q", declaration)
		}
	}
	if strings.Contains(sql, "DELETE FROM admin_override_approvals") {
		t.Fatal("single-operator migration must preserve historical approval rows")
	}
}

func TestWineTicketMoneyContractProfileChecksNullabilityAndConstraints(t *testing.T) {
	columns := make(map[moneyContractColumn]string, len(requiredWineTicketMoneyContractColumns))
	for column, nullable := range requiredWineTicketMoneyContractColumns {
		columns[column] = nullable
	}
	constraints := make(map[string]bool, len(requiredWineTicketMoneyContractConstraints))
	for _, constraint := range requiredWineTicketMoneyContractConstraints {
		constraints[constraint] = true
	}
	if err := validateWineTicketMoneyContract(columns, constraints); err != nil {
		t.Fatalf("expected complete money CONTRACT to pass: %v", err)
	}

	columns[moneyContractColumn{table: "payments", column: "order_id"}] = "NO"
	err := validateWineTicketMoneyContract(columns, constraints)
	if err == nil || !strings.Contains(err.Error(), "payments.order_id") || !strings.Contains(err.Error(), "manual schema-only CONTRACT") {
		t.Fatalf("expected legacy payment linkage to fail closed, got %v", err)
	}

	columns[moneyContractColumn{table: "payments", column: "order_id"}] = "YES"
	delete(constraints, "chk_refund_business_link")
	err = validateWineTicketMoneyContract(columns, constraints)
	if err == nil || !strings.Contains(err.Error(), "chk_refund_business_link") {
		t.Fatalf("expected missing registry constraint to fail closed, got %v", err)
	}
}

func TestMySQLTimeZoneRequiresExplicitPlusEight(t *testing.T) {
	if err := validateMySQLTimeZone("+08:00", "+08:00", "+08:00"); err != nil {
		t.Fatalf("expected aligned timezone to pass, got %v", err)
	}
	for _, tc := range []struct {
		session string
		global  string
	}{
		{session: "SYSTEM", global: "+08:00"},
		{session: "+08:00", global: "SYSTEM"},
		{session: "+00:00", global: "+00:00"},
	} {
		if err := validateMySQLTimeZone(tc.session, tc.global, "+08:00"); err == nil {
			t.Fatalf("expected timezone mismatch to fail: %+v", tc)
		}
	}
}
