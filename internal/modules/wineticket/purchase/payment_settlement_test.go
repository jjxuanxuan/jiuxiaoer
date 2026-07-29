package purchase

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestPurchasePaymentSuccessIssuesExactlyOnce(t *testing.T) {
	service, db, now := newCustomerAssetTestService(t)
	purchase := seedCustomerAssetPurchase(t, db, now, 101, 1, 2, 6)
	if err := db.Create(&PurchaseQuota{
		ID:               501,
		CustomerID:       purchase.CustomerID,
		PackageCode:      "STOCKPILE_A",
		ReservedQuantity: 2,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	paidAt := time.Date(2026, 7, 27, 8, 45, 12, 987654321, time.UTC)
	fact := order.PaymentSettlementFact{
		PaymentID:      purchase.PaymentID,
		PaymentNo:      "PAY-101",
		BizType:        PurchasePaymentBusiness,
		BizID:          purchase.ID,
		CustomerID:     purchase.CustomerID,
		Amount:         purchase.PayableAmount,
		Currency:       "CNY",
		Provider:       "wechat",
		ProviderStatus: "SUCCESS",
		PaidAt:         &paidAt,
	}
	apply := func() error {
		return db.Transaction(func(tx *gorm.DB) error {
			if err := service.LockBusiness(
				context.Background(),
				tx,
				purchase.ID,
			); err != nil {
				return err
			}
			return service.ApplySuccess(context.Background(), tx, fact)
		})
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if err := apply(); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}

	var stored Purchase
	if err := db.First(&stored, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != PurchaseStatusIssued ||
		stored.PaidAt == nil ||
		stored.IssuedAt == nil {
		t.Fatalf("purchase was not issued: %+v", stored)
	}
	var lots []core.Lot
	if err := db.Where("purchase_id = ?", purchase.ID).Find(&lots).Error; err != nil {
		t.Fatal(err)
	}
	if len(lots) != 1 || lots[0].AvailableQuantity != 12 {
		t.Fatalf("lots=%+v, want one 12-bottle lot", lots)
	}
	wantExpiry := paidAt.In(shanghaiLocation).
		Truncate(time.Millisecond).
		AddDate(0, 0, 365)
	if !lots[0].ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expires_at=%s, want=%s", lots[0].ExpiresAt, wantExpiry)
	}
	var entries []core.Transaction
	if err := db.Where("lot_id = ?", lots[0].ID).Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 ||
		entries[0].TransactionType != TransactionTypePurchaseIssue ||
		entries[0].QuantityDelta != 12 ||
		entries[0].BeforeAvailableQuantity != 0 ||
		entries[0].AfterAvailableQuantity != 12 {
		t.Fatalf("issuance ledger=%+v", entries)
	}
	var quota PurchaseQuota
	if err := db.First(&quota, 501).Error; err != nil {
		t.Fatal(err)
	}
	if quota.ReservedQuantity != 0 || quota.ConsumedQuantity != 2 {
		t.Fatalf("quota=%+v, want reserved=0 consumed=2", quota)
	}
}

func TestIssuedPurchaseReplayRejectsChangedImmutableLineage(t *testing.T) {
	service, db, now := newCustomerAssetTestService(t)
	purchase := seedCustomerAssetPurchase(t, db, now, 111, 11, 1, 6)
	if err := db.Create(&PurchaseQuota{
		ID:               511,
		CustomerID:       purchase.CustomerID,
		PackageCode:      "STOCKPILE_A",
		ReservedQuantity: 1,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	paidAt := now.Add(-time.Hour)
	fact := order.PaymentSettlementFact{
		PaymentID:      purchase.PaymentID,
		PaymentNo:      "PAY-111",
		BizType:        PurchasePaymentBusiness,
		BizID:          purchase.ID,
		CustomerID:     purchase.CustomerID,
		Amount:         purchase.PayableAmount,
		Currency:       "CNY",
		Provider:       "wechat",
		ProviderStatus: "SUCCESS",
		PaidAt:         &paidAt,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := service.LockBusiness(
			context.Background(),
			tx,
			purchase.ID,
		); err != nil {
			return err
		}
		return service.ApplySuccess(context.Background(), tx, fact)
	}); err != nil {
		t.Fatal(err)
	}

	var issued Purchase
	if err := db.First(&issued, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	var lot core.Lot
	if err := db.Where("purchase_id = ?", purchase.ID).Take(&lot).Error; err != nil {
		t.Fatal(err)
	}
	var entry core.Transaction
	if err := db.Where(
		"action_key = ?",
		purchaseIssueActionKey(purchase.ID, lot.ID),
	).Take(&entry).Error; err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		model  any
		id     uint64
		column string
		value  any
	}{
		{
			name:  "lot issuer",
			model: &core.Lot{}, id: lot.ID,
			column: "issuer_merchant_id", value: purchase.IssuerMerchantID + 1,
		},
		{
			name:  "lot product",
			model: &core.Lot{}, id: lot.ID,
			column: "product_id", value: purchase.ProductID + 1,
		},
		{
			name:  "lot redeem city",
			model: &core.Lot{}, id: lot.ID,
			column: "redeem_city_code", value: "440100",
		},
		{
			name:  "transaction business id",
			model: &core.Transaction{}, id: entry.ID,
			column: "biz_id", value: purchase.ID + 1,
		},
		{
			name:  "transaction before balance",
			model: &core.Transaction{}, id: entry.ID,
			column: "before_available_quantity", value: 1,
		},
		{
			name:  "transaction after balance",
			model: &core.Transaction{}, id: entry.ID,
			column: "after_available_quantity", value: purchase.TotalBottleQuantity - 1,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			if err := tx.Model(testCase.model).
				Where("id = ?", testCase.id).
				UpdateColumn(testCase.column, testCase.value).Error; err != nil {
				t.Fatal(err)
			}
			if err := service.verifyIssuedPurchase(
				context.Background(),
				tx,
				issued,
			); err == nil {
				t.Fatal("changed immutable issuance lineage was accepted")
			}
		})
	}
}

func TestPurchasePaymentSuccessAfterValidityCreatesCompensation(
	t *testing.T,
) {
	service, db, now := newCustomerAssetTestService(t)
	purchase := seedCustomerAssetPurchase(t, db, now, 102, 2, 1, 6)
	if err := db.Create(&PurchaseQuota{
		ID:               502,
		CustomerID:       purchase.CustomerID,
		PackageCode:      "STOCKPILE_A",
		ReservedQuantity: 1,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	paidAt := now.AddDate(0, 0, -366)
	providerTradeNo := "WX-PAID-102"
	fact := order.PaymentSettlementFact{
		PaymentID:         purchase.PaymentID,
		PaymentNo:         "PAY-102",
		BizType:           PurchasePaymentBusiness,
		BizID:             purchase.ID,
		CustomerID:        purchase.CustomerID,
		Amount:            purchase.PayableAmount,
		Currency:          "CNY",
		Provider:          "wechat",
		ProviderStatus:    "SUCCESS",
		ProviderTradeNo:   &providerTradeNo,
		PaidAt:            &paidAt,
		ReconcileAttempts: 3,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := service.LockBusiness(
			context.Background(),
			tx,
			purchase.ID,
		); err != nil {
			return err
		}
		return service.ApplySuccess(context.Background(), tx, fact)
	}); err != nil {
		t.Fatal(err)
	}

	var stored Purchase
	if err := db.First(&stored, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != PurchaseStatusRefundHolding ||
		stored.PaidAt == nil ||
		stored.PaidAmount != purchase.PayableAmount ||
		stored.IssuedAt != nil {
		t.Fatalf("purchase did not enter compensation: %+v", stored)
	}
	var business issuanceCompensationRefund
	if err := db.Where("purchase_id = ?", purchase.ID).
		Take(&business).Error; err != nil {
		t.Fatal(err)
	}
	if business.RefundKind != RefundKindIssueCompensation ||
		business.Status != RefundStatusHolding ||
		business.Amount != purchase.PayableAmount {
		t.Fatalf("business compensation=%+v", business)
	}
	var common commonRefundRow
	if err := db.First(&common, business.CurrentRefundID).Error; err != nil {
		t.Fatal(err)
	}
	if common.Status != "creating" ||
		common.BizType == nil ||
		*common.BizType != WineTicketPurchaseRefundBusiness ||
		common.BizID == nil ||
		*common.BizID != business.ID {
		t.Fatalf("common compensation=%+v", common)
	}
	var snapshot issuanceCompensationEligibilitySnapshot
	if err := json.Unmarshal(business.EligibilitySnapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.SettlementErrorCode != "validity_elapsed_before_issuance" ||
		snapshot.SettlementAttempts != 3 ||
		snapshot.LotCount != 0 ||
		snapshot.AllocationCount != 0 ||
		snapshot.IssueTransactionCount != 0 {
		t.Fatalf("compensation evidence=%+v", snapshot)
	}
}

func TestPurchaseQuotaSerializesReservationsAndChecksAmount(t *testing.T) {
	service, db, now := newCustomerAssetTestService(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.Create(&PurchaseQuota{
		ID:          700,
		CustomerID:  77,
		PackageCode: "LIMITED",
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	limit := uint(1)
	start := make(chan struct{})
	var succeeded atomic.Int32
	var rejected atomic.Int32
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			err := db.Transaction(func(tx *gorm.DB) error {
				row, err := service.repo.LockPurchaseQuota(
					context.Background(),
					tx,
					77,
					"LIMITED",
				)
				if err != nil {
					return err
				}
				if err := validatePurchaseQuota(row, &limit, 1); err != nil {
					return err
				}
				return service.repo.UpdatePurchaseQuota(
					context.Background(),
					tx,
					row.ID,
					map[string]any{
						"reserved_quantity": row.ReservedQuantity + 1,
						"version":           gorm.Expr("version + 1"),
					},
				)
			})
			if err == nil {
				succeeded.Add(1)
				return
			}
			var details *problem.Details
			if errors.As(err, &details) &&
				details.ErrorCode == "WT_PACKAGE_NOT_AVAILABLE" {
				rejected.Add(1)
				return
			}
			t.Errorf("unexpected reservation error: %v", err)
		}()
	}
	close(start)
	group.Wait()
	if succeeded.Load() != 1 || rejected.Load() != 1 {
		t.Fatalf(
			"succeeded=%d rejected=%d, want 1/1",
			succeeded.Load(),
			rejected.Load(),
		)
	}
	if amount, bottles, err := checkedPurchaseTotals(
		50_000_000,
		6,
		2,
	); err != nil || amount != 100_000_000 || bottles != 12 {
		t.Fatalf("boundary total=(%d,%d,%v)", amount, bottles, err)
	}
}

func TestPurchaseProjectionExcludesGiftReceiverLineage(t *testing.T) {
	service, db, now := newCustomerAssetTestService(t)
	purchase := seedCustomerAssetPurchase(t, db, now, 201, 10, 1, 6)
	expiresAt := now.AddDate(0, 0, 365)
	sourceID := uint64(1001)
	giftID := uint64(9001)
	lots := []core.Lot{
		{
			ID:                sourceID,
			LotNo:             "WTL_BUYER",
			OwnerCustomerID:   10,
			PurchaseID:        purchase.ID,
			SourceType:        LotSourcePurchase,
			IssuerMerchantID:  1,
			ProductID:         401,
			RedeemCityCode:    "310100",
			TotalQuantity:     4,
			AvailableQuantity: 4,
			OriginalExpiresAt: expiresAt,
			ExpiresAt:         expiresAt,
			ExpiryChangedAt:   now,
			Status:            LotStatusActive,
			Version:           1,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
		{
			ID:                1002,
			LotNo:             "WTL_RECEIVER_SECRET",
			OwnerCustomerID:   99,
			PurchaseID:        purchase.ID,
			SourceType:        core.LotSourceGift,
			SourceLotID:       &sourceID,
			SourceGiftID:      &giftID,
			IssuerMerchantID:  1,
			ProductID:         401,
			RedeemCityCode:    "310100",
			TotalQuantity:     2,
			AvailableQuantity: 2,
			OriginalExpiresAt: expiresAt,
			ExpiresAt:         expiresAt,
			ExpiryChangedAt:   now,
			Status:            LotStatusActive,
			Version:           1,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
	}
	if err := db.Create(&lots).Error; err != nil {
		t.Fatal(err)
	}
	dto, err := service.Purchase(
		context.Background(),
		customerClaimsFor(10, "wine_ticket_purchase:view"),
		purchase.PurchaseNo,
	)
	if err != nil {
		t.Fatal(err)
	}
	if dto.RemainingBottleQuantity != 4 ||
		dto.LotCount != 1 ||
		len(dto.LotSummaries) != 1 ||
		dto.LotSummaries[0].LotNo != "WTL_BUYER" {
		t.Fatalf("buyer projection includes wrong lineage: %+v", dto)
	}
	raw, _ := json.Marshal(dto)
	if strings.Contains(string(raw), "WTL_RECEIVER_SECRET") {
		t.Fatalf("receiver lot leaked in purchaser response: %s", raw)
	}
}
