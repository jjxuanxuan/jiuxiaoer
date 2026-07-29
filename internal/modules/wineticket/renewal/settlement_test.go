package renewal

import (
	"context"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func seedPendingRenewal(
	t *testing.T,
	db *gorm.DB,
	now time.Time,
	customerID, purchaseID, lotID, renewalID, paymentID uint64,
	fee int64,
) (core.Lot, Renewal, order.Payment) {
	t.Helper()
	_, lot := seedRenewalLot(
		t,
		db,
		now,
		customerID,
		purchaseID,
		lotID,
		fee,
		2,
	)
	if err := db.Model(&core.Lot{}).Where("id = ?", lot.ID).Updates(map[string]any{
		"ever_used": true,
		"version":   2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	lot.EverUsed = true
	lot.Version = 2
	newExpiry := renewalNewExpiry(lot.ExpiresAt, 30)
	renewal := Renewal{
		ID:                 renewalID,
		RenewalNo:          "WTRN" + idString(renewalID),
		LotID:              lot.ID,
		CustomerID:         customerID,
		PaymentID:          &paymentID,
		OldExpiresAt:       lot.ExpiresAt,
		NewExpiresAt:       newExpiry,
		ExtensionDays:      30,
		FeeAmount:          fee,
		Currency:           "CNY",
		PolicySnapshot:     paidRenewalPolicy(fee),
		ExpectedLotVersion: 1,
		Status:             RenewalStatusPendingPayment,
		Version:            1,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	bizType := RenewalPaymentBusiness
	bizID := renewal.ID
	payment := order.Payment{
		ID:         paymentID,
		PaymentNo:  "WTRPAY" + idString(paymentID),
		BizType:    &bizType,
		BizID:      &bizID,
		CustomerID: customerID,
		Channel:    "wechat_miniapp",
		Provider:   "wechat",
		Status:     "pending",
		Amount:     fee,
		Currency:   "CNY",
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := db.Create(&renewal).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&payment).Error; err != nil {
		t.Fatal(err)
	}
	return lot, renewal, payment
}

func paidRenewalPolicy(fee int64) []byte {
	return []byte(
		`{"schema_version":1,"enabled":true,"extension_days":30,"max_count":2,"grace_days":0,"fee_amount":` +
			strconv.FormatInt(fee, 10) +
			`}`,
	)
}

func TestLatePaidRenewalCompensatesAndRefundSuccessExpiresGuardedLot(
	t *testing.T,
) {
	db := newRenewalTestDB(t)
	seedNow := time.Date(
		2026,
		7,
		27,
		10,
		0,
		0,
		123000000,
		shanghaiLocation,
	)
	ids := snowflake.New(303)
	serviceNow := seedNow
	service := NewRenewalService(
		db,
		ids,
		renewalTestQuoteSecret,
	).WithRenewalClock(func() time.Time { return serviceNow })
	lot, renewal, payment := seedPendingRenewal(
		t,
		db,
		seedNow,
		9701,
		9711,
		9721,
		9731,
		9741,
		990,
	)
	serviceNow = lot.ExpiresAt.Add(time.Hour)
	paidAt := lot.ExpiresAt.Add(time.Millisecond)
	fact := order.PaymentSettlementFact{
		PaymentID:       payment.ID,
		PaymentNo:       payment.PaymentNo,
		BizType:         RenewalPaymentBusiness,
		BizID:           renewal.ID,
		CustomerID:      renewal.CustomerID,
		Amount:          renewal.FeeAmount,
		Currency:        renewal.Currency,
		Provider:        "wechat",
		ProviderStatus:  "SUCCESS",
		ProviderTradeNo: stringPointer("WX-LATE-RENEWAL"),
		PaidAt:          &paidAt,
	}
	paymentHandler := NewRenewalPaymentSettlementHandler(service)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return paymentHandler.ApplySuccess(context.Background(), tx, fact)
	}); err != nil {
		t.Fatal(err)
	}

	var compensating Renewal
	if err := db.First(&compensating, "id = ?", renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if compensating.Status != RenewalStatusCompensatingRefund ||
		compensating.CompensatingRefundID == nil ||
		compensating.CompletedAt != nil {
		t.Fatalf("unexpected compensation renewal: %+v", compensating)
	}
	var unchanged core.Lot
	if err := db.First(&unchanged, "id = ?", lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.RenewalCount != 0 ||
		!unchanged.ExpiresAt.Equal(lot.ExpiresAt) ||
		unchanged.AvailableQuantity != lot.AvailableQuantity {
		t.Fatalf("late payment must not apply renewal: %+v", unchanged)
	}

	refundService := refund.NewService(
		config.Config{},
		db,
		ids,
		nil,
	).WithRefundSettlementHandler(
		NewRenewalRefundSettlementHandler(service),
	)
	var compensation refund.Row
	if err := db.First(
		&compensation,
		"id = ?",
		*compensating.CompensatingRefundID,
	).Error; err != nil {
		t.Fatal(err)
	}
	refundState := refund.State{
		ProviderRefundID: "WX-RF-LATE-RENEWAL",
		RefundNo:         compensation.RefundNo,
		PaymentNo:        payment.PaymentNo,
		Status:           "SUCCESS",
		Currency:         "CNY",
		Amount:           compensation.Amount,
		TotalAmount:      compensation.TotalAmount,
		CurrencyRequired: true,
		SucceededAt:      &serviceNow,
	}
	for index := 0; index < 100; index++ {
		if err := refundService.ApplyProviderState(
			context.Background(),
			compensation.ID,
			refundState,
		); err != nil {
			t.Fatalf("refund replay %d: %v", index, err)
		}
	}

	var finalRenewal Renewal
	var finalLot core.Lot
	var finalPayment order.Payment
	var finalRefund refund.Row
	if err := db.First(&finalRenewal, "id = ?", renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&finalLot, "id = ?", lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&finalPayment, "id = ?", payment.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&finalRefund, "id = ?", compensation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if finalRenewal.Status != RenewalStatusRefunded ||
		finalLot.RenewalCount != 0 ||
		finalLot.Status != LotStatusExpired ||
		finalLot.AvailableQuantity != 0 ||
		finalPayment.RefundedAmount != renewal.FeeAmount ||
		finalRefund.Status != "succeeded" {
		t.Fatalf(
			"renewal=%+v lot=%+v payment=%+v refund=%+v",
			finalRenewal,
			finalLot,
			finalPayment,
			finalRefund,
		)
	}
	var transactions []core.Transaction
	if err := db.Find(&transactions).Error; err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 1 ||
		transactions[0].TransactionType != transactionTypeLotExpiry ||
		transactions[0].QuantityDelta != -int(lot.AvailableQuantity) ||
		transactions[0].QuantityDelta == 0 {
		t.Fatalf("unexpected guard-release ledger: %+v", transactions)
	}
}

func TestRenewalTerminalAndUnknownStatesNeverLeakGuardOrZeroLedger(
	t *testing.T,
) {
	t.Run("payment unknown retains guard", func(t *testing.T) {
		db := newRenewalTestDB(t)
		now := time.Date(2026, 7, 27, 11, 0, 0, 0, shanghaiLocation)
		ids := snowflake.New(304)
		service := NewRenewalService(
			db,
			ids,
			renewalTestQuoteSecret,
		).WithRenewalClock(func() time.Time { return now })
		lot, renewal, payment := seedPendingRenewal(
			t,
			db,
			now,
			9801,
			9811,
			9821,
			9831,
			9841,
			660,
		)
		handler := NewRenewalPaymentSettlementHandler(service)
		fact := order.PaymentSettlementFact{
			PaymentID:      payment.ID,
			PaymentNo:      payment.PaymentNo,
			BizType:        RenewalPaymentBusiness,
			BizID:          renewal.ID,
			CustomerID:     renewal.CustomerID,
			Amount:         renewal.FeeAmount,
			Currency:       renewal.Currency,
			Provider:       "wechat",
			ProviderStatus: "UNKNOWN",
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			return handler.ApplyException(
				context.Background(),
				tx,
				fact,
				"PROVIDER_UNKNOWN",
			)
		}); err != nil {
			t.Fatal(err)
		}
		var stored Renewal
		if err := db.First(&stored, "id = ?", renewal.ID).Error; err != nil {
			t.Fatal(err)
		}
		if stored.Status != RenewalStatusPaymentUnknown {
			t.Fatalf("unknown payment released guard: %+v", stored)
		}
		if _, err := service.repo.activeRenewal(
			context.Background(),
			nil,
			lot.ID,
			false,
		); err != nil {
			t.Fatalf("payment_unknown guard missing: %v", err)
		}
	})

	t.Run("explicit close releases and expires once", func(t *testing.T) {
		db := newRenewalTestDB(t)
		seedNow := time.Date(2026, 7, 27, 12, 0, 0, 0, shanghaiLocation)
		ids := snowflake.New(305)
		serviceNow := seedNow
		service := NewRenewalService(
			db,
			ids,
			renewalTestQuoteSecret,
		).WithRenewalClock(func() time.Time { return serviceNow })
		lot, renewal, payment := seedPendingRenewal(
			t,
			db,
			seedNow,
			9901,
			9911,
			9921,
			9931,
			9941,
			770,
		)
		serviceNow = lot.ExpiresAt
		handler := NewRenewalPaymentSettlementHandler(service)
		fact := order.PaymentSettlementFact{
			PaymentID:      payment.ID,
			PaymentNo:      payment.PaymentNo,
			BizType:        RenewalPaymentBusiness,
			BizID:          renewal.ID,
			CustomerID:     renewal.CustomerID,
			Amount:         renewal.FeeAmount,
			Currency:       renewal.Currency,
			Provider:       "wechat",
			ProviderStatus: "CLOSED",
		}
		for index := 0; index < 2; index++ {
			if err := db.Transaction(func(tx *gorm.DB) error {
				return handler.ApplyTerminal(context.Background(), tx, fact)
			}); err != nil {
				t.Fatalf("close replay %d: %v", index, err)
			}
		}
		var stored Renewal
		var expired core.Lot
		if err := db.First(&stored, "id = ?", renewal.ID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.First(&expired, "id = ?", lot.ID).Error; err != nil {
			t.Fatal(err)
		}
		var ledgerCount int64
		db.Model(&core.Transaction{}).Count(&ledgerCount)
		if stored.Status != RenewalStatusClosed ||
			expired.Status != LotStatusExpired ||
			expired.AvailableQuantity != 0 ||
			expired.RenewalCount != 0 ||
			ledgerCount != 1 {
			t.Fatalf(
				"renewal=%+v lot=%+v ledgers=%d",
				stored,
				expired,
				ledgerCount,
			)
		}
	})
}

func TestCompensationUnknownAndClosedRetainGuard(t *testing.T) {
	db := newRenewalTestDB(t)
	seedNow := time.Date(2026, 7, 27, 13, 0, 0, 0, shanghaiLocation)
	ids := snowflake.New(306)
	serviceNow := seedNow
	service := NewRenewalService(
		db,
		ids,
		renewalTestQuoteSecret,
	).WithRenewalClock(func() time.Time { return serviceNow })
	lot, renewal, payment := seedPendingRenewal(
		t,
		db,
		seedNow,
		9951,
		9961,
		9971,
		9981,
		9991,
		550,
	)
	serviceNow = lot.ExpiresAt.Add(time.Hour)
	latePaidAt := lot.ExpiresAt.Add(time.Millisecond)
	paymentHandler := NewRenewalPaymentSettlementHandler(service)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return paymentHandler.ApplySuccess(
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
	var stored Renewal
	if err := db.First(&stored, "id = ?", renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	var originalRefund refund.Row
	if err := db.First(
		&originalRefund,
		"id = ?",
		*stored.CompensatingRefundID,
	).Error; err != nil {
		t.Fatal(err)
	}
	refundService := refund.NewService(
		config.Config{},
		db,
		ids,
		nil,
	).WithRefundSettlementHandler(
		NewRenewalRefundSettlementHandler(service),
	)
	unknown := refund.State{
		RefundNo:    originalRefund.RefundNo,
		PaymentNo:   payment.PaymentNo,
		Status:      "UNKNOWN",
		Currency:    "CNY",
		Amount:      originalRefund.Amount,
		TotalAmount: originalRefund.TotalAmount,
	}
	if err := refundService.ApplyProviderState(
		context.Background(),
		originalRefund.ID,
		unknown,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&stored, "id = ?", renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != RenewalStatusRefundException {
		t.Fatalf("UNKNOWN released compensation guard: %+v", stored)
	}
	var stillGuarded core.Lot
	if err := db.First(&stillGuarded, "id = ?", lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stillGuarded.Status != LotStatusActive ||
		stillGuarded.AvailableQuantity != lot.AvailableQuantity {
		t.Fatalf("UNKNOWN expired a guarded lot: %+v", stillGuarded)
	}

	closed := unknown
	closed.Status = "CLOSED"
	if err := refundService.ApplyProviderState(
		context.Background(),
		originalRefund.ID,
		closed,
	); err != nil {
		t.Fatal(err)
	}
	if err := refundService.ApplyProviderState(
		context.Background(),
		originalRefund.ID,
		closed,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&stored, "id = ?", renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	var refundCount int64
	db.Model(&refund.Row{}).
		Where("biz_type = ? AND biz_id = ?", RenewalCompensationRefundBusiness, renewal.ID).
		Count(&refundCount)
	if stored.Status != RenewalStatusCompensatingRefund ||
		stored.CompensatingRefundID == nil ||
		*stored.CompensatingRefundID == originalRefund.ID ||
		refundCount != 2 {
		t.Fatalf("renewal=%+v refund_count=%d", stored, refundCount)
	}
}
