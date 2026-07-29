package renewal

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestRenewalRefundWorkerFailuresPreserveCompensationGuard(t *testing.T) {
	db := newRenewalTestDB(t)
	now := time.Date(2026, 7, 27, 19, 0, 0, 0, shanghaiLocation)
	ids := snowflake.New(322)
	renewalService := NewRenewalService(
		db,
		ids,
		renewalTestQuoteSecret,
	).WithRenewalClock(func() time.Time { return now })
	lot, renewal, payment := seedPendingRenewal(
		t,
		db,
		now,
		10901,
		10911,
		10921,
		10931,
		10941,
		660,
	)

	// 延迟支付会创建受保护的补偿退款。
	latePaidAt := lot.ExpiresAt.Add(time.Millisecond)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return NewRenewalPaymentSettlementHandler(renewalService).ApplySuccess(
			context.Background(),
			tx,
			order.PaymentSettlementFact{
				PaymentID:      payment.ID,
				PaymentNo:      payment.PaymentNo,
				BizType:        RenewalPaymentBusiness,
				BizID:          renewal.ID,
				CustomerID:     renewal.CustomerID,
				Amount:         renewal.FeeAmount,
				Currency:       renewal.Currency,
				Provider:       "wechat",
				ProviderStatus: "SUCCESS",
				PaidAt:         &latePaidAt,
			},
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&order.Payment{}).
		Where("id = ?", payment.ID).
		Updates(map[string]any{
			"status":          "succeeded",
			"provider_status": "SUCCESS",
			"paid_at":         latePaidAt,
		}).Error; err != nil {
		t.Fatal(err)
	}

	var guarded Renewal
	if err := db.First(&guarded, renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if guarded.Status != RenewalStatusCompensatingRefund ||
		guarded.CompensatingRefundID == nil {
		t.Fatalf("late payment did not create compensation guard: %+v", guarded)
	}
	var common refund.Row
	if err := db.First(&common, *guarded.CompensatingRefundID).Error; err != nil {
		t.Fatal(err)
	}
	shared := refund.NewService(
		config.Config{},
		db,
		ids,
		nil,
	).WithRefundSettlementHandler(
		NewRenewalRefundSettlementHandler(renewalService),
	)

	claimedCreatingVersion := common.Version
	if err := shared.MarkAttemptError(
		context.Background(),
		common.ID,
		claimedCreatingVersion,
		errors.New("wechat refund request timed out"),
	); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&common, common.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&guarded, renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	var untouched core.Lot
	if err := db.First(&untouched, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if common.Status != "submission_unknown" ||
		guarded.Status != RenewalStatusCompensatingRefund ||
		untouched.Status != lot.Status ||
		untouched.AvailableQuantity != lot.AvailableQuantity ||
		!untouched.ExpiresAt.Equal(lot.ExpiresAt) {
		t.Fatalf(
			"retryable failure leaked guard: refund=%+v renewal=%+v lot=%+v",
			common,
			guarded,
			untouched,
		)
	}

	// 陈旧的后台任务结果不得覆盖可重试观测。
	if err := shared.MarkPermanentError(
		context.Background(),
		common.ID,
		claimedCreatingVersion,
		"WECHAT_REFUND_REJECTED",
		errors.New("stale worker result"),
	); err != nil {
		t.Fatal(err)
	}
	var staleCheck refund.Row
	if err := db.First(&staleCheck, common.ID).Error; err != nil {
		t.Fatal(err)
	}
	if staleCheck.Status != "submission_unknown" {
		t.Fatalf("stale worker changed refund: %+v", staleCheck)
	}

	if err := shared.MarkPermanentError(
		context.Background(),
		common.ID,
		common.Version,
		"WECHAT_REFUND_REJECTED",
		errors.New("wechat rejected the refund"),
	); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&common, common.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&guarded, renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&untouched, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if common.Status != "exception" ||
		guarded.Status != RenewalStatusRefundException ||
		guarded.CompensatingRefundID == nil ||
		*guarded.CompensatingRefundID != common.ID ||
		untouched.Status != lot.Status ||
		untouched.AvailableQuantity != lot.AvailableQuantity ||
		!untouched.ExpiresAt.Equal(lot.ExpiresAt) {
		t.Fatalf(
			"permanent failure released guard: refund=%+v renewal=%+v lot=%+v",
			common,
			guarded,
			untouched,
		)
	}
}
