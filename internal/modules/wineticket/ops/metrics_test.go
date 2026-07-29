package ops

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	refundmodule "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/integrity"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/reminder"
	renewaldomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
)

func TestWineTicketMetricsExposeMoneyEntitlementAndWorkerLag(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open(uniqueSQLiteMemoryDSN(t)),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&core.Transaction{}, &refunddomain.WineTicketRefund{},
		&redemption.Redemption{}, &gift.Gift{},
		&integrity.Exception{}, &reminder.Reminder{}, &renewaldomain.Renewal{},
		&integrity.Checkpoint{},
		&order.Payment{}, &refundmodule.Row{},
	); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE payments ADD COLUMN deleted_at DATETIME`,
		`ALTER TABLE refunds ADD COLUMN deleted_at DATETIME`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	bizType := purchase.PurchasePaymentBusiness
	bizID := uint64(11)
	providerStatus := "SUCCESS"
	paidAt := now.Add(-2 * time.Minute)
	if err := db.Create(&order.Payment{
		ID: 1, PaymentNo: "PAY-1", BizType: &bizType, BizID: &bizID,
		Provider: "wechat", ProviderStatus: &providerStatus,
		Status: "exception", PaidAt: &paidAt, UpdatedAt: paidAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&core.Transaction{
		ID: 2, TransactionNo: "WTT2", LotID: 3, OwnerCustomerID: 4,
		TransactionType: core.TransactionTypePurchaseIssue,
		BizType:         "purchase", BizID: 11, ActionKey: "purchase_issue:11:3",
		CreatedAt: paidAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	commonStatus := "PROCESSING"
	if err := db.Create(&refundmodule.Row{
		ID: 5, RefundNo: "RF-5", Provider: "wechat", Status: "pending",
		ProviderStatus: &commonStatus, RequestedAt: now.Add(-3 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&refunddomain.WineTicketRefund{
		ID: 6, WineTicketRefundNo: "WTR6", PurchaseID: 11, CustomerID: 4,
		CurrentRefundID: 5, RefundKind: "user_unused",
		Status:      refunddomain.RefundStatusHolding,
		RequestedAt: now.Add(-3 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemption.Redemption{
		ID:     7,
		Status: "scheduled",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&gift.Gift{
		ID:     8,
		Status: "claimed",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&reminder.Reminder{
		ID: 9, LotID: 3, OwnerCustomerID: 4, Channel: "wechat_subscription",
		Status: "pending", ScheduledAt: now.Add(-5 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&renewaldomain.Renewal{
		ID: 10, RenewalNo: "WTRN10", LotID: 3,
		Status:    renewaldomain.RenewalStatusPaymentUnknown,
		CreatedAt: now.Add(-4 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	correlationID := integrity.RuleLotReplay
	if err := db.Create(&integrity.Exception{
		ID: 11, ExceptionNo: "WTEX11", ExceptionType: "REC-WT-003:replay",
		BizType: "lot", BizID: 3, SourceType: "wine_ticket_reconciliation",
		CorrelationID: &correlationID, Status: ExceptionStatusInvestigating,
	}).Error; err != nil {
		t.Fatal(err)
	}

	samples := collectWineTicketMetrics(db, now)
	values := make(map[string]float64, len(samples))
	for _, sample := range samples {
		key := sample.Name
		for label, value := range sample.Labels {
			key += "|" + label + "=" + value
		}
		values[key] = sample.Value
	}
	for key, minimum := range map[string]float64{
		"jxe_wine_ticket_issue_total|result=succeeded":                         1,
		"jxe_wine_ticket_redemption_total|result=scheduled":                    1,
		"jxe_wine_ticket_gift_claim_total|result=claimed":                      1,
		"jxe_wine_ticket_lot_invariant_violation_total":                        1,
		"jxe_wine_ticket_reconcile_diff_total|type=REC-WT-003":                 1,
		"jxe_wine_ticket_settlement_lag_seconds|biz_type=wine_ticket_purchase": 120,
		"jxe_wine_ticket_reminder_lag_seconds|channel=wechat_subscription":     300,
		"jxe_wine_ticket_refund_hold_age_seconds|provider_status=PROCESSING":   180,
		"jxe_wine_ticket_renewal_guard_age_seconds":                            240,
		"jxe_wine_ticket_reconciliation_deadline_missed":                       1,
	} {
		if values[key] < minimum {
			t.Errorf("metric %s=%f want >=%f; all=%v", key, values[key], minimum, values)
		}
	}
}
