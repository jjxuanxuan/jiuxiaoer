package purchase

import (
	"context"
	"net/http"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

func TestPurchaseRecoveryAfterPaymentSubmissionSkipsMutableEligibility(
	t *testing.T,
) {
	service, db, now := newCustomerAssetTestService(t)
	if err := db.Exec(`
		CREATE UNIQUE INDEX uk_purchase_recovery_idempotency
		ON idempotency_keys(actor_type, actor_id, path, key_hash)
	`).Error; err != nil {
		t.Fatal(err)
	}
	purchase := seedCustomerAssetPurchase(t, db, now, 8_101, 8_201, 1, 2)
	key := "purchase-recovery-0001"
	quota := PurchaseQuota{
		ID:               8_401,
		CustomerID:       purchase.CustomerID,
		PackageCode:      "STOCKPILE_A",
		ReservedQuantity: 1,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := db.Create(&quota).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&order.Payment{}).
		Where("id = ?", purchase.PaymentID).
		Update("idempotency_key", key).Error; err != nil {
		t.Fatal(err)
	}

	request := PurchaseCreateRequest{
		PackageNo: "WTP" + idString(purchase.ID),
		Quantity:  1,
	}
	path := "/api/v1/wine-ticket/purchases"
	expiredLease := now.Add(-time.Minute)
	if err := db.Create(&idempotency.Record{
		ID:          8_301,
		ActorType:   "customer",
		ActorID:     purchase.CustomerID,
		Method:      http.MethodPost,
		Path:        path,
		KeyHash:     idempotency.KeyHash(key),
		RequestHash: idempotency.RequestHash(request),
		Status:      "processing",
		LockedUntil: &expiredLease,
		ExpiredAt:   now.Add(24 * time.Hour),
		CreatedAt:   now.Add(-2 * time.Minute),
		UpdatedAt:   expiredLease,
	}).Error; err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{}
	cfg.WeChat.PayEnabled = true
	cfg.WeChat.PayMockEnabled = true
	cfg.WeChat.HTTPTimeout = time.Second
	paymentService := order.NewService(cfg, db, service.ids).
		WithPaymentProvider(
			&purchasePaymentProvider{},
			metrics.New("purchase-recovery", ""),
		)
	service.WithPaymentService(paymentService).WithWeChatAppID("wx-purchase-test")
	paymentService.WithPaymentSettlementHandler(service)

	// 有意不创建客户、身份或实名记录。
	// 支付机构提交已进入 pending，恢复流程必须使用不可变的支付和购买草稿，
	// 而不是当前可变资格。
	claims := customerClaimsFor(
		purchase.CustomerID,
		"wine_ticket_purchase:create",
	)
	first, err := service.CreatePurchase(
		context.Background(),
		claims,
		http.MethodPost,
		path,
		key,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.PurchaseNo != purchase.PurchaseNo ||
		first.PaymentStatus != "pending" ||
		len(first.PaymentParameters) == 0 {
		t.Fatalf("recovered purchase=%+v", first)
	}

	replayed, err := service.CreatePurchase(
		context.Background(),
		claims,
		http.MethodPost,
		path,
		key,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.PurchaseNo != first.PurchaseNo ||
		replayed.PaymentStatus != first.PaymentStatus {
		t.Fatalf("cached replay=%+v first=%+v", replayed, first)
	}

	var purchaseCount, paymentCount int64
	if err := db.Model(&Purchase{}).Count(&purchaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&order.Payment{}).Count(&paymentCount).Error; err != nil {
		t.Fatal(err)
	}
	if purchaseCount != 1 || paymentCount != 1 {
		t.Fatalf(
			"purchases=%d payments=%d, want one recovered draft",
			purchaseCount,
			paymentCount,
		)
	}
	var recoveredQuota PurchaseQuota
	if err := db.First(&recoveredQuota, quota.ID).Error; err != nil {
		t.Fatal(err)
	}
	if recoveredQuota.ReservedQuantity != quota.ReservedQuantity {
		t.Fatalf(
			"reserved_quantity=%d, want unchanged %d",
			recoveredQuota.ReservedQuantity,
			quota.ReservedQuantity,
		)
	}
}

func TestPurchaseRecoveryConvergesTerminalPaymentAndReleasesQuota(
	t *testing.T,
) {
	service, db, now := newCustomerAssetTestService(t)
	if err := db.Exec(`
		CREATE UNIQUE INDEX uk_purchase_terminal_recovery_idempotency
		ON idempotency_keys(actor_type, actor_id, path, key_hash)
	`).Error; err != nil {
		t.Fatal(err)
	}
	purchase := seedCustomerAssetPurchase(t, db, now, 9_101, 9_201, 1, 2)
	key := "purchase-recovery-terminal-0001"
	quota := PurchaseQuota{
		ID:               9_401,
		CustomerID:       purchase.CustomerID,
		PackageCode:      "STOCKPILE_A",
		ReservedQuantity: 1,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := db.Create(&quota).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&order.Payment{}).
		Where("id = ?", purchase.PaymentID).
		Updates(map[string]any{
			"idempotency_key": key,
			"status":          "failed",
			"client_payload":  nil,
		}).Error; err != nil {
		t.Fatal(err)
	}

	request := PurchaseCreateRequest{
		PackageNo: "WTP" + idString(purchase.ID),
		Quantity:  1,
	}
	path := "/api/v1/wine-ticket/purchases"
	expiredLease := now.Add(-time.Minute)
	if err := db.Create(&idempotency.Record{
		ID:          9_301,
		ActorType:   "customer",
		ActorID:     purchase.CustomerID,
		Method:      http.MethodPost,
		Path:        path,
		KeyHash:     idempotency.KeyHash(key),
		RequestHash: idempotency.RequestHash(request),
		Status:      "processing",
		LockedUntil: &expiredLease,
		ExpiredAt:   now.Add(24 * time.Hour),
		CreatedAt:   now.Add(-2 * time.Minute),
		UpdatedAt:   expiredLease,
	}).Error; err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{}
	cfg.WeChat.PayEnabled = true
	cfg.WeChat.PayMockEnabled = true
	cfg.WeChat.HTTPTimeout = time.Second
	paymentService := order.NewService(cfg, db, service.ids).
		WithPaymentProvider(
			&purchasePaymentProvider{},
			metrics.New("purchase-terminal-recovery", ""),
		)
	service.WithPaymentService(paymentService).WithWeChatAppID("wx-purchase-test")
	paymentService.WithPaymentSettlementHandler(service)

	// 有意省略可变的客户、身份和实名记录。
	// 已持久化的支付终态必须关闭恢复出的购买记录并释放配额，
	// 且不能再次向支付机构提交。
	claims := customerClaimsFor(
		purchase.CustomerID,
		"wine_ticket_purchase:create",
	)
	first, err := service.CreatePurchase(
		context.Background(),
		claims,
		http.MethodPost,
		path,
		key,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.PurchaseNo != purchase.PurchaseNo ||
		first.Status != PurchaseStatusClosed ||
		first.PaymentStatus != "failed" {
		t.Fatalf("terminal recovered purchase=%+v", first)
	}

	var recoveredPurchase Purchase
	if err := db.First(&recoveredPurchase, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	if recoveredPurchase.Status != PurchaseStatusClosed {
		t.Fatalf(
			"purchase status=%q, want %q",
			recoveredPurchase.Status,
			PurchaseStatusClosed,
		)
	}
	var recoveredQuota PurchaseQuota
	if err := db.First(&recoveredQuota, quota.ID).Error; err != nil {
		t.Fatal(err)
	}
	if recoveredQuota.ReservedQuantity != 0 {
		t.Fatalf(
			"reserved_quantity=%d, want released quota",
			recoveredQuota.ReservedQuantity,
		)
	}

	replayed, err := service.CreatePurchase(
		context.Background(),
		claims,
		http.MethodPost,
		path,
		key,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.PurchaseNo != first.PurchaseNo ||
		replayed.Status != PurchaseStatusClosed ||
		replayed.PaymentStatus != "failed" {
		t.Fatalf("cached replay=%+v first=%+v", replayed, first)
	}

	var purchaseCount, paymentCount int64
	if err := db.Model(&Purchase{}).Count(&purchaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&order.Payment{}).Count(&paymentCount).Error; err != nil {
		t.Fatal(err)
	}
	if purchaseCount != 1 || paymentCount != 1 {
		t.Fatalf(
			"purchases=%d payments=%d, want one recovered draft",
			purchaseCount,
			paymentCount,
		)
	}
}
