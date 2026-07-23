package order

import (
	"context"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestOrderActionFailureAuditContainsOnlySafeErrorMetadata(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&AuditLog{}); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.Config{}, db, snowflake.New(82))
	claims := &auth.Claims{AccountType: "customer", CustomerID: "42"}
	if err := service.AuditFailure(context.Background(), claims, "payment_confirm", "99", problem.Conflict("PAYMENT_STATE_CONFLICT", "provider trade secret wx-123")); err != nil {
		t.Fatal(err)
	}
	var row AuditLog
	if err := db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Result != "failed" || row.ActorID != 42 || row.ResourceID != 99 || !strings.Contains(string(row.AfterData), "PAYMENT_STATE_CONFLICT") {
		t.Fatalf("unexpected failure audit: %+v", row)
	}
	if strings.Contains(string(row.AfterData), "wx-123") {
		t.Fatalf("failure audit leaked provider data: %s", row.AfterData)
	}
}
