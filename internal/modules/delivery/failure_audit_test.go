package delivery

import (
	"context"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestDeliveryActionFailureIsAuditedWithoutSensitiveInput(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&AuditLog{}); err != nil {
		t.Fatal(err)
	}
	service := NewService(db, snowflake.New(81))
	claims := &auth.Claims{AccountType: "rider", RiderID: "42"}
	if err := service.AuditFailure(context.Background(), claims, "delivery_complete", "99", problem.Conflict("DELIVERY_INVALID_STATUS", "delivery code 123456 rejected")); err != nil {
		t.Fatal(err)
	}
	var row AuditLog
	if err := db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Result != "failed" || row.ActorID != 42 || row.ResourceID != 99 || !strings.Contains(string(row.AfterData), "DELIVERY_INVALID_STATUS") {
		t.Fatalf("unexpected failure audit: %+v", row)
	}
	if strings.Contains(string(row.AfterData), "123456") {
		t.Fatalf("failure audit leaked verification code: %s", row.AfterData)
	}
}
