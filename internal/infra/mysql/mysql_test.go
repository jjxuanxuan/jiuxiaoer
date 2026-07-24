package mysql

import (
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
