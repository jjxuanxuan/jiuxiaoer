package order

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestCreateBusinessPaymentPersistsNullRetailOrderLink(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Payment{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE payments ADD COLUMN deleted_at datetime").Error; err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{}
	cfg.WeChat.PayEnabled = true
	service := NewService(cfg, db, snowflake.New(96)).
		WithPaymentProvider(&paymentSettlementCallbackProvider{}, metrics.New("business-payment-test", "")).
		WithPaymentSettlementHandler(&paymentSettlementTestHandler{})

	expiresAt := time.Now().Add(15 * time.Minute)
	err = db.Transaction(func(tx *gorm.DB) error {
		_, err := service.CreateBusinessPaymentTx(context.Background(), tx, BusinessPaymentCreateInput{
			PaymentID: 2501, PaymentNo: "WT-PAY-2501",
			BizType: "wine_ticket_purchase", BizID: 2401, CustomerID: 2301,
			Channel: "wechat_miniapp", Provider: "wechat",
			Amount: 18800, Currency: "CNY", ExpiresAt: expiresAt,
			IdempotencyKey: "wine-ticket-create-2501",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	var orderID sql.NullInt64
	if err := db.Table("payments").Select("order_id").Where("id = ?", 2501).Row().Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if orderID.Valid {
		t.Fatalf("non-retail payment must persist NULL order_id, got %d", orderID.Int64)
	}
	var payment Payment
	if err := db.First(&payment, "id = ?", 2501).Error; err != nil {
		t.Fatal(err)
	}
	if payment.OrderID != nil || payment.BizType == nil || *payment.BizType != "wine_ticket_purchase" {
		t.Fatalf("unexpected business payment: %+v", payment)
	}
}
