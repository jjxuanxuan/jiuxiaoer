package purchase

import (
	"strconv"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// AC-WT-005E：资金链路进入或完成退款处理后，
// 100 个延迟失败事实只能作为无害证据，
// 不得让购买记录回退到发放结算异常状态。
func TestMySQLLatePurchaseSettlementFailureNeverDowngradesFundsTerminalState(
	t *testing.T,
) {
	ctx, db := openWineTicketMySQLAcceptance(t, 45*time.Second)
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	ids := snowflake.New(947)

	for _, terminalStatus := range []string{
		PurchaseStatusRefundHolding,
		PurchaseStatusRefundException,
		PurchaseStatusRefunded,
	} {
		t.Run(terminalStatus, func(t *testing.T) {
			purchase, quota, fact := mysqlPurchaseFixture(
				t,
				db,
				ids,
				now,
				"MYSQL-LATE-FAILURE-"+terminalStatus+"-"+idString(ids.Next()),
			)
			t.Cleanup(func() {
				cleanupMySQLPurchaseAcceptance(t, db, purchase, quota)
			})
			purchaseUpdates := map[string]any{
				"status": terminalStatus, "paid_amount": purchase.PayableAmount,
				"paid_at": now.Add(-time.Hour), "updated_at": now,
			}
			if terminalStatus == PurchaseStatusRefunded {
				purchaseUpdates["refunded_at"] = now
			}
			if err := db.Model(&Purchase{}).Where("id = ?", purchase.ID).
				UpdateColumns(purchaseUpdates).Error; err != nil {
				t.Fatalf("seed purchase terminal status %s: %v", terminalStatus, err)
			}
			if err := db.Model(&PurchaseQuota{}).Where("id = ?", quota.ID).
				UpdateColumns(map[string]any{
					"reserved_quantity": 0,
					"consumed_quantity": purchase.PackageQuantity,
					"updated_at":        now,
				}).Error; err != nil {
				t.Fatalf("seed consumed quota: %v", err)
			}

			service := NewService(db, ids)
			service.now = func() time.Time { return now }
			results := runMySQLConcurrentErrors(
				mysqlP0Concurrency,
				func(_ int) error {
					return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
						if err := service.LockBusiness(ctx, tx, purchase.ID); err != nil {
							return err
						}
						return service.ApplyException(
							ctx,
							tx,
							fact,
							"delayed_provider_failure_after_funds_terminal",
						)
					})
				},
			)
			for _, resultErr := range results {
				if resultErr != nil {
					t.Fatalf("late failure on %s: %v", terminalStatus, resultErr)
				}
			}

			var stored Purchase
			if err := db.First(&stored, purchase.ID).Error; err != nil {
				t.Fatal(err)
			}
			var storedQuota PurchaseQuota
			if err := db.First(&storedQuota, quota.ID).Error; err != nil {
				t.Fatal(err)
			}
			var downgradeAudits int64
			if err := db.Table("audit_logs").Where(
				"resource_type = ? AND resource_id = ? AND action = ?",
				"wine_ticket_purchase",
				purchase.ID,
				"wine_ticket.purchase.settlement_exception",
			).Count(&downgradeAudits).Error; err != nil {
				t.Fatal(err)
			}
			if stored.Status != terminalStatus || stored.Version != purchase.Version ||
				storedQuota.ReservedQuantity != 0 ||
				storedQuota.ConsumedQuantity != purchase.PackageQuantity ||
				downgradeAudits != 0 {
				t.Fatalf(
					"late failure downgraded funds state purchase=%+v quota=%+v audits=%d",
					stored,
					storedQuota,
					downgradeAudits,
				)
			}
		})
	}
}

// AC-WT-005C：存在任何部分权益事实时，自动补偿必须失败关闭，
// 绝不能创建第二份权益或自动退款。
func TestMySQLIssuanceCompensationRejectsPartialEntitlementFacts(t *testing.T) {
	ctx, db := openWineTicketMySQLAcceptance(t, 30*time.Second)
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	ids := snowflake.New(942)

	cases := []struct {
		name string
		seed func(Purchase)
	}{
		{
			name: "lot_without_issue_transaction",
			seed: func(purchase Purchase) {
				expiresAt := now.AddDate(1, 0, 0)
				row := core.Lot{
					ID: ids.Next(), LotNo: "MYSQL-PARTIAL-LOT-" + idString(ids.Next()),
					OwnerCustomerID: purchase.CustomerID, PurchaseID: purchase.ID,
					SourceType: LotSourcePurchase, IssuerMerchantID: purchase.IssuerMerchantID,
					ProductID: purchase.ProductID, RedeemCityCode: purchase.RedeemCityCode,
					TotalQuantity: 1, AvailableQuantity: 1,
					OriginalExpiresAt: expiresAt, ExpiresAt: expiresAt,
					ExpiryChangedAt: now, Status: LotStatusActive, Version: 1,
					CreatedAt: now, UpdatedAt: now,
				}
				if err := db.Create(&row).Error; err != nil {
					t.Fatalf("seed partial lot: %v", err)
				}
			},
		},
		{
			name: "issue_transaction_without_lot",
			seed: func(purchase Purchase) {
				id := ids.Next()
				row := core.Transaction{
					ID: id, TransactionNo: "MYSQL-PARTIAL-TX-" + idString(id),
					LotID: ids.Next(), OwnerCustomerID: purchase.CustomerID,
					TransactionType: TransactionTypePurchaseIssue,
					QuantityDelta:   1, BeforeAvailableQuantity: 0,
					AfterAvailableQuantity: 1, BizType: "purchase",
					BizID:     purchase.ID,
					ActionKey: "mysql-partial-issue-" + idString(purchase.ID),
					CreatedAt: now,
				}
				if err := db.Create(&row).Error; err != nil {
					t.Fatalf("seed partial issue transaction: %v", err)
				}
			},
		},
		{
			name: "refund_allocation_without_complete_refund",
			seed: func(purchase Purchase) {
				refundID := ids.Next()
				refund := issuanceCompensationRefund{
					ID:                 refundID,
					WineTicketRefundNo: "MYSQL-PARTIAL-REFUND-" + idString(refundID),
					PurchaseID:         purchase.ID, CustomerID: purchase.CustomerID,
					CurrentRefundID: ids.Next(), RefundKind: "user_unused",
					Amount: purchase.PayableAmount, Currency: "CNY",
					ReasonCode:          "partial_fixture",
					EligibilitySnapshot: datatypes.JSON(`{"schema_version":1}`),
					Status:              "cancelled", Version: 1,
					RequestedAt: now, CreatedAt: now, UpdatedAt: now,
				}
				if err := db.Create(&refund).Error; err != nil {
					t.Fatalf("seed partial business refund: %v", err)
				}
				allocation := purchaseRefundAllocationGuard{
					ID: ids.Next(), WineTicketRefundID: refundID, LotID: ids.Next(),
					Quantity: 1, SourceExpiresAt: now.AddDate(1, 0, 0),
					Status: "exception", CreatedAt: now, UpdatedAt: now,
				}
				if err := db.Create(&allocation).Error; err != nil {
					t.Fatalf("seed partial refund allocation: %v", err)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			purchase, quota, fact := mysqlPurchaseFixture(
				t,
				db,
				ids,
				now,
				"MYSQL-PARTIAL-"+strconv.FormatUint(ids.Next(), 10),
			)
			t.Cleanup(func() {
				cleanupMySQLPurchaseAcceptance(t, db, purchase, quota)
			})
			testCase.seed(purchase)
			expiredPaidAt := now.AddDate(-2, 0, 0)
			fact.PaidAt = &expiredPaidAt
			service := NewService(db, ids)
			service.now = func() time.Time { return now }

			err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := service.LockBusiness(ctx, tx, purchase.ID); err != nil {
					return err
				}
				return service.ApplySuccess(ctx, tx, fact)
			})
			if err == nil || problem.FromError(err).ErrorCode != "INTERNAL_ERROR" {
				t.Fatalf("partial entitlement did not fail closed: %v", err)
			}

			var stored Purchase
			if err := db.First(&stored, purchase.ID).Error; err != nil {
				t.Fatal(err)
			}
			var storedQuota PurchaseQuota
			if err := db.First(&storedQuota, quota.ID).Error; err != nil {
				t.Fatal(err)
			}
			var compensationCount int64
			if err := db.Model(&issuanceCompensationRefund{}).Where(
				"purchase_id = ? AND refund_kind = ?",
				purchase.ID,
				RefundKindIssueCompensation,
			).Count(&compensationCount).Error; err != nil {
				t.Fatal(err)
			}
			if stored.Status != PurchaseStatusPendingPayment ||
				storedQuota.ReservedQuantity != 1 ||
				storedQuota.ConsumedQuantity != 0 ||
				compensationCount != 0 {
				t.Fatalf(
					"partial facts crossed compensation guard: purchase=%+v quota=%+v compensations=%d",
					stored,
					storedQuota,
					compensationCount,
				)
			}
		})
	}
}
