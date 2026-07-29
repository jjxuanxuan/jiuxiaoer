package aftersale

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type acceptanceFixture struct {
	orderID, itemID, secondItemID, paymentID, stockID, shopProductID uint64
	customerID, merchantID, merchantUserID, shopID                   uint64
}

// TestL3AfterSaleAcceptance 验证L 3 售后销售验收的预期行为。
func TestL3AfterSaleAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run L3 acceptance tests")
	}
	db := openAcceptanceDB(t)
	ids := snowflake.New(992)
	cfg := config.Load()
	cfg.AfterSale.Enabled = true
	cfg.AfterSale.RefundExecutionEnabled = true
	cfg.AfterSale.PlatformReviewThreshold = 1_000_000

	t.Run("ACC-L3-001-002-005-006-007-008-009", func(t *testing.T) {
		tx := rollbackTx(t, db)
		fx := insertAcceptanceOrder(t, tx, ids, "completed", 2, 1000, time.Now().Add(-time.Hour))
		service := NewService(cfg, tx, ids)
		owner := customerClaims(fx.customerID)
		req := CreateReq{OrderID: idString(fx.orderID), Type: "damaged", RequestedResolution: "refund_only", Items: []CreateItemReq{{OrderItemID: idString(fx.itemID), Quantity: 2, RequestedAmount: 1000}}, Description: "bottle damaged on arrival", EvidenceTokens: []string{acceptanceEvidenceToken(t, cfg, fx.customerID, ids.Next())}}

		created, err := service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "create-acceptance-001", req)
		if err != nil || created.Status != "submitted" || created.RequestedAmount != 1000 || len(created.Items) != 1 {
			t.Fatalf("ACC-001 create failed: dto=%+v err=%v", created, err)
		}
		if _, err := service.DetailCustomer(context.Background(), customerClaims(fx.customerID+999), created.ID); errorCode(err) != "AFTER_SALE_NOT_FOUND" {
			t.Fatalf("ACC-002 expected hidden 404, got %v", err)
		}

		for index := 0; index < 10; index++ {
			repeated, err := service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "create-acceptance-001", req)
			if err != nil || repeated.ID != created.ID {
				t.Fatalf("ACC-006 repeat %d: dto=%+v err=%v", index, repeated, err)
			}
		}
		var itemCount, eventCount int64
		tx.Table("after_sale_items").Where("after_sale_id=?", mustID(t, created.ID)).Count(&itemCount)
		tx.Table("outbox_events").Where("aggregate_type='after_sale' AND aggregate_id=? AND event_type='after_sale.submitted'", mustID(t, created.ID)).Count(&eventCount)
		if itemCount != 1 || eventCount != 1 {
			t.Fatalf("ACC-006 duplicates: items=%d events=%d", itemCount, eventCount)
		}

		duplicateReq := req
		duplicateReq.EvidenceTokens = []string{acceptanceEvidenceToken(t, cfg, fx.customerID, ids.Next())}
		_, err = service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "create-acceptance-007", duplicateReq)
		if errorCode(err) != "AFTER_SALE_DUPLICATE_ACTIVE" {
			t.Fatalf("ACC-007 got %v", err)
		}
		overReq := req
		overReq.Items[0].Quantity = 2
		overReq.Items[0].RequestedAmount = 1001
		overReq.EvidenceTokens = []string{acceptanceEvidenceToken(t, cfg, fx.customerID, ids.Next())}
		_, err = service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "create-acceptance-008", overReq)
		if code := errorCode(err); code != "AFTER_SALE_DUPLICATE_ACTIVE" && code != "AFTER_SALE_AMOUNT_EXCEEDED" {
			t.Fatalf("ACC-008 got %v", err)
		}

		withdrawn, err := service.Withdraw(context.Background(), owner, "POST", "/api/v1/after-sales/:id/withdraw", "withdraw-acceptance-009", created.ID, WithdrawReq{Reason: "resolved", Version: created.Version})
		if err != nil || withdrawn.Status != "withdrawn" {
			t.Fatalf("ACC-009 dto=%+v err=%v", withdrawn, err)
		}
		var refunds int64
		tx.Table("refunds").Where("after_sale_id=?", mustID(t, created.ID)).Count(&refunds)
		if refunds != 0 {
			t.Fatalf("ACC-009 reserved refund count=%d", refunds)
		}

		_, err = service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "create-no-evidence", CreateReq{OrderID: idString(fx.orderID), Type: "damaged", RequestedResolution: "refund_only", Items: []CreateItemReq{{OrderItemID: idString(fx.secondItemID), Quantity: 1, RequestedAmount: 500}}, Description: "damaged without evidence"})
		if errorCode(err) != "AFTER_SALE_EVIDENCE_REQUIRED" {
			t.Fatalf("ACC-005 got %v", err)
		}
	})

	t.Run("ACC-L3-003-004", func(t *testing.T) {
		tx := rollbackTx(t, db)
		service := NewService(cfg, tx, ids)
		unpaid := insertAcceptanceOrder(t, tx, ids, "pending_payment", 1, 1000, time.Now())
		_, err := service.Create(context.Background(), customerClaims(unpaid.customerID), "POST", "/api/v1/after-sales", "unpaid-acceptance", basicCreateReq(t, cfg, ids, unpaid, "damaged", "refund_only"))
		if errorCode(err) != "AFTER_SALE_NOT_ELIGIBLE" {
			t.Fatalf("ACC-003 got %v", err)
		}
		cancelled := insertAcceptanceOrder(t, tx, ids, "cancelled", 1, 1000, time.Now())
		_, err = service.Create(context.Background(), customerClaims(cancelled.customerID), "POST", "/api/v1/after-sales", "cancelled-acceptance", basicCreateReq(t, cfg, ids, cancelled, "damaged", "refund_only"))
		if errorCode(err) != "AFTER_SALE_NOT_ELIGIBLE" {
			t.Fatalf("ACC-003 cancelled got %v", err)
		}
		expired := insertAcceptanceOrder(t, tx, ids, "completed", 1, 1000, time.Now().Add(-8*24*time.Hour))
		_, err = service.Create(context.Background(), customerClaims(expired.customerID), "POST", "/api/v1/after-sales", "expired-acceptance", basicCreateReq(t, cfg, ids, expired, "unopened_return", "return_and_refund"))
		if errorCode(err) != "AFTER_SALE_NOT_ELIGIBLE" {
			t.Fatalf("ACC-004 got %v", err)
		}
		over := insertAcceptanceOrder(t, tx, ids, "completed", 1, 1000, time.Now().Add(-time.Hour))
		overReq := basicCreateReq(t, cfg, ids, over, "damaged", "refund_only")
		overReq.Items[0].Quantity = 2
		overReq.Items[0].RequestedAmount = 1001
		_, err = service.Create(context.Background(), customerClaims(over.customerID), "POST", "/api/v1/after-sales", "amount-over-acceptance", overReq)
		if errorCode(err) != "AFTER_SALE_AMOUNT_EXCEEDED" {
			t.Fatalf("ACC-008 got %v", err)
		}
	})

	t.Run("ACC-L3-010-011-012-013-014-015-016-020", func(t *testing.T) {
		tx := rollbackTx(t, db)
		service := NewService(cfg, tx, ids)
		fx := insertAcceptanceOrder(t, tx, ids, "completed", 2, 1000, time.Now().Add(-time.Hour))
		owner := customerClaims(fx.customerID)
		created, err := service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "review-create-001", basicCreateReq(t, cfg, ids, fx, "damaged", "refund_only"))
		if err != nil {
			t.Fatal(err)
		}
		merchant := merchantClaims(fx)
		otherShop := merchant
		otherShop.AuthorizedShopIDs = []string{idString(fx.shopID + 999)}
		if _, err := service.ReviewStore(context.Background(), &otherShop, "POST", "/api/v1/store/after-sales/:id/review", "wrong-shop-review", created.ID, ReviewReq{Decision: "approve", Resolution: "refund_only", Version: created.Version}); errorCode(err) != "AFTER_SALE_NOT_FOUND" {
			t.Fatalf("ACC-011 got %v", err)
		}

		itemID := created.Items[0].ID
		reviewed, err := service.ReviewStore(context.Background(), &merchant, "POST", "/api/v1/store/after-sales/:id/review", "own-shop-review", created.ID, ReviewReq{Decision: "approve", Resolution: "refund_only", ApprovedItems: []ApprovedItemReq{{AfterSaleItemID: itemID, Quantity: 1, Amount: 500}}, Version: created.Version})
		if err != nil || reviewed.Status != "refund_processing" {
			t.Fatalf("ACC-010/020 dto=%+v err=%v", reviewed, err)
		}
		var history, audits, refunds int64
		tx.Table("after_sale_history").Where("after_sale_id=? AND action='review_approve'", mustID(t, created.ID)).Count(&history)
		tx.Table("audit_logs").Where("resource_type='after_sale' AND resource_id=? AND action='after_sale.approve'", mustID(t, created.ID)).Count(&audits)
		tx.Table("refunds").Where("after_sale_id=? AND currency='CNY' AND amount=500", mustID(t, created.ID)).Count(&refunds)
		if history != 1 || audits != 1 || refunds != 1 {
			t.Fatalf("ACC-010/020 history=%d audits=%d refunds=%d", history, audits, refunds)
		}
		if _, err := service.ReviewStore(context.Background(), &merchant, "POST", "/api/v1/store/after-sales/:id/review", "stale-review", created.ID, ReviewReq{Decision: "approve", Resolution: "refund_only", Version: created.Version}); errorCode(err) != "AFTER_SALE_VERSION_CONFLICT" {
			t.Fatalf("ACC-012 got %v", err)
		}

		secondReq := basicCreateReq(t, cfg, ids, fx, "damaged", "refund_only")
		secondReq.Items[0].OrderItemID = idString(fx.secondItemID)
		second, err := service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "review-create-002", secondReq)
		if err != nil {
			t.Fatal(err)
		}
		admin := adminClaims(ids.Next(), true)
		_, err = service.ReviewAdmin(context.Background(), &admin, "POST", "/api/v1/admin/after-sales/:id/review", "over-review", second.ID, ReviewReq{Decision: "approve", Resolution: "refund_only", ApprovedItems: []ApprovedItemReq{{AfterSaleItemID: second.Items[0].ID, Quantity: 2, Amount: 1001}}, Version: second.Version})
		if errorCode(err) != "AFTER_SALE_AMOUNT_EXCEEDED" {
			t.Fatalf("ACC-013 got %v", err)
		}
		var secondRefunds int64
		tx.Table("refunds").Where("after_sale_id=?", mustID(t, second.ID)).Count(&secondRefunds)
		if secondRefunds != 0 {
			t.Fatalf("ACC-013 created refund")
		}

		escalated := second
		if escalated.Status != "platform_reviewing" {
			t.Fatalf("ACC-014 expected automatic escalation, got %s", escalated.Status)
		}
		platform, err := service.ReviewAdmin(context.Background(), &admin, "POST", "/api/v1/admin/after-sales/:id/review", "platform-review", second.ID, ReviewReq{Decision: "reject", Remark: "claim not supported", Version: escalated.Version})
		if err != nil || platform.Status != "rejected" {
			t.Fatalf("ACC-014 dto=%+v err=%v", platform, err)
		}
		noPermission := adminClaims(ids.Next(), false)
		if _, err := service.ReviewAdmin(context.Background(), &noPermission, "POST", "/api/v1/admin/after-sales/:id/review", "no-permission", second.ID, ReviewReq{Decision: "reject", Remark: "denied", Version: platform.Version}); errorCode(err) != "PERM_FORBIDDEN" {
			t.Fatalf("ACC-015 got %v", err)
		}
		appealed, err := service.Appeal(context.Background(), owner, "POST", "/api/v1/after-sales/:id/appeal", "appeal-once", second.ID, AppealReq{Remark: "please review this claim", Version: platform.Version})
		if err != nil || appealed.Status != "platform_reviewing" {
			t.Fatalf("ACC-016 dto=%+v err=%v", appealed, err)
		}
		if _, err := service.Appeal(context.Background(), owner, "POST", "/api/v1/after-sales/:id/appeal", "appeal-twice", second.ID, AppealReq{Remark: "second appeal attempt", Version: appealed.Version}); errorCode(err) != "AFTER_SALE_STATUS_CONFLICT" {
			t.Fatalf("ACC-016 second got %v", err)
		}
	})

	t.Run("ACC-L3-017-018-019-033-034", func(t *testing.T) {
		tx := rollbackTx(t, db)
		service := NewService(cfg, tx, ids)
		merchantID := ids.Next()
		_ = merchantID
		for _, disposition := range []string{"restock", "damaged"} {
			fx := insertAcceptanceOrder(t, tx, ids, "completed", 1, 1000, time.Now().Add(-time.Hour))
			created, err := service.Create(context.Background(), customerClaims(fx.customerID), "POST", "/api/v1/after-sales", "return-create-"+disposition+idString(fx.orderID), basicCreateReq(t, cfg, ids, fx, "unopened_return", "return_and_refund"))
			if err != nil {
				t.Fatal(err)
			}
			merchant := merchantClaims(fx)
			approved, err := service.ReviewStore(context.Background(), &merchant, "POST", "/api/v1/store/after-sales/:id/review", "return-review-"+disposition+idString(fx.orderID), created.ID, ReviewReq{Decision: "approve", Resolution: "return_and_refund", Version: created.Version})
			if err != nil {
				t.Fatal(err)
			}
			var before int
			tx.Table("product_stocks").Select("available_qty").Where("id=?", fx.stockID).Scan(&before)
			_, err = service.ReceiveReturn(context.Background(), &merchant, "POST", "/api/v1/store/after-sales/:id/return-receipts", "return-receipt-"+disposition+idString(fx.orderID), created.ID, ReturnReceiptReq{Disposition: disposition, SealedPackageIntact: true, GoodsIntact: true, Version: approved.Version})
			if err != nil {
				t.Fatal(err)
			}
			var after int
			tx.Table("product_stocks").Select("available_qty").Where("id=?", fx.stockID).Scan(&after)
			want := before
			if disposition == "restock" {
				want++
			}
			if after != want {
				t.Fatalf("ACC-017/018 disposition=%s before=%d after=%d", disposition, before, after)
			}
			var records int64
			tx.Table("stock_records").Where("source_type='after_sale_return' AND shop_product_id=?", fx.shopProductID).Count(&records)
			if disposition == "restock" && records != 1 {
				t.Fatalf("ACC-017 records=%d", records)
			}
			if disposition == "damaged" && records != 0 {
				t.Fatalf("ACC-018 records=%d", records)
			}
		}

		replacementFX := insertAcceptanceOrder(t, tx, ids, "completed", 1, 1000, time.Now().Add(-time.Hour))
		originalSnapshot := `{"contact_name":"original"}`
		if err := tx.Exec("UPDATE orders SET address_snapshot=CAST(? AS JSON) WHERE id=?", originalSnapshot, replacementFX.orderID).Error; err != nil {
			t.Fatal(err)
		}
		var snapshotBefore string
		tx.Table("orders").Select("CAST(address_snapshot AS CHAR)").Where("id=?", replacementFX.orderID).Scan(&snapshotBefore)
		created, err := service.Create(context.Background(), customerClaims(replacementFX.customerID), "POST", "/api/v1/after-sales", "replacement-create", basicCreateReq(t, cfg, ids, replacementFX, "damaged", "replacement"))
		if err != nil {
			t.Fatal(err)
		}
		merchant := merchantClaims(replacementFX)
		approved, err := service.ReviewStore(context.Background(), &merchant, "POST", "/api/v1/store/after-sales/:id/review", "replacement-review", created.ID, ReviewReq{Decision: "approve", Resolution: "replacement", Version: created.Version})
		if err != nil {
			t.Fatal(err)
		}
		replacement, err := service.ReserveReplacement(context.Background(), &merchant, "POST", "/api/v1/store/after-sales/:id/replacements", "replacement-reserve", created.ID, ReplacementReq{Version: approved.Version})
		if err != nil || replacement.Status != "stock_reserved" {
			t.Fatalf("ACC-019 dto=%+v err=%v", replacement, err)
		}
		var currentSnapshot string
		tx.Table("orders").Select("CAST(address_snapshot AS CHAR)").Where("id=?", replacementFX.orderID).Scan(&currentSnapshot)
		if currentSnapshot != snapshotBefore {
			t.Fatalf("ACC-019 original order snapshot changed: before=%s after=%s", snapshotBefore, currentSnapshot)
		}

		feeFX := insertAcceptanceOrder(t, tx, ids, "completed", 2, 1000, time.Now().Add(-time.Hour))
		tx.Table("orders").Where("id=?", feeFX.orderID).Update("delivery_fee_amount", 100)
		feeReq := basicCreateReq(t, cfg, ids, feeFX, "out_of_stock", "refund_only")
		feeReq.IncludeDeliveryFee = true
		if _, err := service.Create(context.Background(), customerClaims(feeFX.customerID), "POST", "/api/v1/after-sales", "fee-first", feeReq); err != nil {
			t.Fatal(err)
		}
		feeReq.Items[0].OrderItemID = idString(feeFX.secondItemID)
		if _, err := service.Create(context.Background(), customerClaims(feeFX.customerID), "POST", "/api/v1/after-sales", "fee-second", feeReq); errorCode(err) != "AFTER_SALE_AMOUNT_EXCEEDED" {
			t.Fatalf("ACC-033 got %v", err)
		}

		compFX := insertAcceptanceOrder(t, tx, ids, "completed", 1, 1000, time.Now().Add(-time.Hour))
		comp, err := service.Create(context.Background(), customerClaims(compFX.customerID), "POST", "/api/v1/after-sales", "comp-create", basicCreateReq(t, cfg, ids, compFX, "late_delivery", "compensation"))
		if err != nil {
			t.Fatal(err)
		}
		admin := adminClaims(ids.Next(), true)
		admin.Permissions = append(admin.Permissions, "compensation:approve")
		approvedComp, err := service.ReviewAdmin(context.Background(), &admin, "POST", "/api/v1/admin/after-sales/:id/review", "comp-review", comp.ID, ReviewReq{Decision: "approve", Resolution: "compensation", Version: comp.Version})
		if err != nil || approvedComp.Status != "compensation_processing" {
			t.Fatalf("ACC-034 dto=%+v err=%v", approvedComp, err)
		}
		var refunded, ledgers int64
		tx.Table("payments").Select("refunded_amount").Where("id=?", compFX.paymentID).Scan(&refunded)
		tx.Table("compensation_ledger").Where("after_sale_id=? AND status='approved'", mustID(t, comp.ID)).Count(&ledgers)
		if refunded != 0 || ledgers != 1 {
			t.Fatalf("ACC-034 refunded=%d ledgers=%d", refunded, ledgers)
		}

		fullFX := insertAcceptanceOrder(t, tx, ids, "completed", 1, 1000, time.Now().Add(-time.Hour))
		if err := tx.Table("payments").Where("id=?", fullFX.paymentID).Update("refunded_amount", 1000).Error; err != nil {
			t.Fatal(err)
		}
		full, err := service.Create(context.Background(), customerClaims(fullFX.customerID), "POST", "/api/v1/after-sales", "fully-refunded-create", basicCreateReq(t, cfg, ids, fullFX, "damaged", "refund_only"))
		if err != nil {
			t.Fatal(err)
		}
		fullMerchant := merchantClaims(fullFX)
		_, err = service.ReviewStore(context.Background(), &fullMerchant, "POST", "/api/v1/store/after-sales/:id/review", "fully-refunded-review", full.ID, ReviewReq{Decision: "approve", Resolution: "refund_only", Version: full.Version})
		if errorCode(err) != "REFUND_AMOUNT_EXCEEDED" {
			t.Fatalf("ACC-032 got %v", err)
		}
	})

	t.Run("ACC-L3-021-concurrent-last-refundable-amount", func(t *testing.T) {
		// 由专用的已提交夹具覆盖，因为并发事务无法共享外层回滚事务。
		testConcurrentApproval(t, db, cfg, ids)
	})
}

// openAcceptanceDB 解密并返回验收数据库。
func openAcceptanceDB(t *testing.T) *gorm.DB {
	t.Helper()
	cfg := config.Load()
	db, err := mysql.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// rollbackTx 返回回拨 Tx。
func rollbackTx(t *testing.T, db *gorm.DB) *gorm.DB {
	t.Helper()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

// insertAcceptanceOrder 插入验收订单。
func insertAcceptanceOrder(t *testing.T, db *gorm.DB, ids *snowflake.Generator, status string, itemCount int, amount int64, completedAt time.Time) acceptanceFixture {
	t.Helper()
	fx := acceptanceFixture{orderID: ids.Next(), paymentID: ids.Next(), stockID: ids.Next(), shopProductID: ids.Next(), customerID: ids.Next(), merchantID: ids.Next(), merchantUserID: ids.Next(), shopID: ids.Next()}
	payStatus := "succeeded"
	paidAmount := amount
	if status == "pending_payment" {
		payStatus = "pending"
		paidAmount = 0
	}
	var completed any = completedAt
	if status != "completed" {
		completed = nil
	}
	mustExec(t, db, "INSERT INTO orders (id,order_no,customer_id,merchant_id,shop_id,status,pay_status,delivery_status,goods_amount,payable_amount,paid_amount,address_snapshot,completed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", fx.orderID, "ACC-ORDER-"+idString(fx.orderID), fx.customerID, fx.merchantID, fx.shopID, status, payStatus, "completed", amount*int64(itemCount), amount*int64(itemCount), paidAmount*int64(itemCount), `{"contact_name":"fixture"}`, completed)
	mustExec(t, db, "INSERT INTO payments (id,payment_no,biz_type,biz_id,order_id,customer_id,channel,provider,status,amount,currency) VALUES (?,?,'retail_order',?,?,?, 'miniapp','wechat',?,?, 'CNY')", fx.paymentID, "ACC-PAY-"+idString(fx.paymentID), fx.orderID, fx.orderID, fx.customerID, payStatus, amount*int64(itemCount))
	mustExec(t, db, "INSERT INTO product_stocks (id,shop_product_id,shop_id,product_id,available_qty,reserved_qty) VALUES (?,?,?,?,10,0)", fx.stockID, fx.shopProductID, fx.shopID, ids.Next())
	for index := 0; index < itemCount; index++ {
		itemID := ids.Next()
		if index == 0 {
			fx.itemID = itemID
		} else if index == 1 {
			fx.secondItemID = itemID
		}
		mustExec(t, db, "INSERT INTO order_items (id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount) VALUES (?,?,?,?,?,2,?,?)", itemID, fx.orderID, fx.shopProductID, ids.Next(), `{"name":"fixture","return_policy":{"eligible":true,"policy_code":"legal","policy_version":"1","sealed_package_required":true}}`, amount/2, amount)
	}
	return fx
}

// basicCreateReq 返回基础创建请求。
func basicCreateReq(t *testing.T, cfg config.Config, ids *snowflake.Generator, fx acceptanceFixture, kind, resolution string) CreateReq {
	t.Helper()
	req := CreateReq{OrderID: idString(fx.orderID), Type: kind, RequestedResolution: resolution, Items: []CreateItemReq{{OrderItemID: idString(fx.itemID), Quantity: 1, RequestedAmount: 500}}, Description: "acceptance test request"}
	if kind == "damaged" {
		req.EvidenceTokens = []string{acceptanceEvidenceToken(t, cfg, fx.customerID, ids.Next())}
	}
	if resolution == "replacement" || resolution == "compensation" {
		req.Items[0].RequestedAmount = 500
	}
	return req
}

// acceptanceEvidenceToken 返回验收 Evidence 令牌。
func acceptanceEvidenceToken(t *testing.T, cfg config.Config, customerID, tokenID uint64) string {
	t.Helper()
	claims := evidenceClaims{ObjectKey: "evidence/" + idString(tokenID), MimeType: "image/jpeg", SizeBytes: 1024, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ScanStatus: "clean", RegisteredClaims: jwt.RegisteredClaims{Issuer: "jxe-upload", Subject: idString(customerID), ID: "token-" + idString(tokenID), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.AfterSale.EvidenceTokenSecret))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// customerClaims 返回用户认证声明。
func customerClaims(id uint64) *auth.Claims {
	return &auth.Claims{AccountType: "customer", CustomerID: idString(id)}
}

// merchantClaims 返回商户认证声明。
func merchantClaims(fx acceptanceFixture) auth.Claims {
	return auth.Claims{AccountType: "merchant", MerchantUserID: idString(fx.merchantUserID), MerchantID: idString(fx.merchantID), AuthorizedShopIDs: []string{idString(fx.shopID)}, Permissions: []string{"after_sale:list_shop", "after_sale:view_shop", "after_sale:review_shop", "after_sale:receive_return", "after_sale:create_replacement"}}
}

// adminClaims 返回管理端认证声明。
func adminClaims(id uint64, allowed bool) auth.Claims {
	permissions := []string{}
	if allowed {
		permissions = []string{"after_sale:list_all", "after_sale:view_all", "after_sale:review_platform", "refund:list", "refund:view", "refund:retry", "refund:exception"}
	}
	return auth.Claims{AccountType: "admin", AdminUserID: idString(id), Permissions: permissions}
}

// errorCode 返回错误代码。
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	return problem.FromError(err).ErrorCode
}

// mustID 解析 ID，失败时终止测试。
func mustID(t *testing.T, raw string) uint64 {
	t.Helper()
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// mustExec 执行 SQL，失败时终止测试。
func mustExec(t *testing.T, db *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := db.Exec(query, args...).Error; err != nil {
		t.Fatalf("fixture SQL: %v", err)
	}
}

// testConcurrentApproval 验证并发审批。
func testConcurrentApproval(t *testing.T, db *gorm.DB, cfg config.Config, ids *snowflake.Generator) {
	fx := insertAcceptanceOrder(t, db, ids, "completed", 2, 500, time.Now().Add(-time.Hour))
	if err := db.Table("payments").Where("id=?", fx.paymentID).Update("amount", 500).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("orders").Where("id=?", fx.orderID).Updates(map[string]any{"paid_amount": 500, "payable_amount": 500}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, db, ids)
	owner := customerClaims(fx.customerID)
	admin := adminClaims(ids.Next(), true)
	firstReq := basicCreateReq(t, cfg, ids, fx, "damaged", "refund_only")
	firstReq.Items[0].Quantity = 2
	firstReq.Items[0].RequestedAmount = 500
	first, err := service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "concurrent-create-1-"+idString(fx.orderID), firstReq)
	if err != nil {
		t.Fatal(err)
	}
	secondReq := firstReq
	secondReq.Items = []CreateItemReq{{OrderItemID: idString(fx.secondItemID), Quantity: 2, RequestedAmount: 500}}
	secondReq.EvidenceTokens = []string{acceptanceEvidenceToken(t, cfg, fx.customerID, ids.Next())}
	second, err := service.Create(context.Background(), owner, "POST", "/api/v1/after-sales", "concurrent-create-2-"+idString(fx.orderID), secondReq)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCommittedAcceptance(t, db, fx)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for index, row := range []DTO{first, second} {
		wg.Add(1)
		go func(index int, row DTO) {
			defer wg.Done()
			_, err := service.ReviewAdmin(context.Background(), &admin, "POST", "/api/v1/admin/after-sales/:id/review", fmt.Sprintf("concurrent-review-%d-%d", index, fx.orderID), row.ID, ReviewReq{Decision: "approve", Resolution: "refund_only", Version: row.Version})
			results <- err
		}(index, row)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errorCode(err) == "REFUND_AMOUNT_EXCEEDED" {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		var rows []struct {
			RefundNo, Status    string
			Amount, TotalAmount int64
		}
		db.Table("refunds").Select("refund_no,status,amount,total_amount").Where("order_id=?", fx.orderID).Order("id").Scan(&rows)
		var payment struct{ Amount, RefundedAmount int64 }
		db.Table("payments").Select("amount,refunded_amount").Where("id=?", fx.paymentID).Scan(&payment)
		t.Fatalf("ACC-021 success=%d conflict=%d payment=%+v refunds=%+v", successes, conflicts, payment, rows)
	}
}

// cleanupCommittedAcceptance 清理Committed 验收。
func cleanupCommittedAcceptance(t *testing.T, db *gorm.DB, fx acceptanceFixture) {
	t.Helper()
	queries := []string{
		"DELETE rc FROM refund_callbacks rc JOIN refunds r ON r.id=rc.refund_id WHERE r.order_id=?", "DELETE ri FROM refund_items ri JOIN refunds r ON r.id=ri.refund_id WHERE r.order_id=?", "DELETE FROM refunds WHERE order_id=?",
		"DELETE rr FROM return_receipts rr JOIN after_sales a ON a.id=rr.after_sale_id WHERE a.order_id=?", "DELETE rp FROM replacement_fulfillments rp JOIN after_sales a ON a.id=rp.after_sale_id WHERE a.order_id=?", "DELETE cp FROM compensation_ledger cp JOIN after_sales a ON a.id=cp.after_sale_id WHERE a.order_id=?",
		"DELETE ev FROM after_sale_evidence ev JOIN after_sales a ON a.id=ev.after_sale_id WHERE a.order_id=?", "DELETE h FROM after_sale_history h JOIN after_sales a ON a.id=h.after_sale_id WHERE a.order_id=?", "DELETE i FROM after_sale_items i JOIN after_sales a ON a.id=i.after_sale_id WHERE a.order_id=?", "DELETE FROM after_sales WHERE order_id=?",
		"DELETE FROM idempotency_keys WHERE actor_id IN (?,?,?)", "DELETE FROM payments WHERE order_id=?", "DELETE FROM order_items WHERE order_id=?", "DELETE FROM product_stocks WHERE id=?", "DELETE FROM orders WHERE id=?",
	}
	for _, query := range queries {
		args := []any{fx.orderID}
		if query == "DELETE FROM idempotency_keys WHERE actor_id IN (?,?,?)" {
			args = []any{fx.customerID, fx.merchantUserID, fx.merchantID}
		}
		if query == "DELETE FROM product_stocks WHERE id=?" {
			args = []any{fx.stockID}
		}
		if err := db.Exec(query, args...).Error; err != nil {
			t.Logf("cleanup: %v", err)
		}
	}
}
