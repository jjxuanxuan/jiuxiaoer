package store

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestStoreOrderActionExpectedVersionIsRequiredAndAllowsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bind := func(body string) (StoreOrderActionReq, error) {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/store/orders/1/accept", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		var request StoreOrderActionReq
		err := ctx.ShouldBindJSON(&request)
		return request, err
	}

	if _, err := bind(`{}`); err == nil {
		t.Fatal("missing expected_version must be rejected")
	}
	request, err := bind(`{"expected_version":0}`)
	if err != nil || request.ExpectedVersion == nil || *request.ExpectedVersion != 0 {
		t.Fatalf("version zero is a present optimistic-lock value: request=%+v err=%v", request, err)
	}
	if _, err := storeOrderExpectedVersion(StoreOrderActionReq{}); problem.FromError(err).ErrorCode != "VALIDATION_FAILED" {
		t.Fatalf("direct service calls must also reject a missing version: %v", err)
	}
}

func TestStoreOrderActionRequestHashBindsObjectActionAndVersion(t *testing.T) {
	version, otherVersion := uint(3), uint(4)
	request := StoreOrderActionReq{ExpectedVersion: &version}
	want := storeOrderActionRequestHash(100, "accept", request)
	if got := storeOrderActionRequestHash(100, "accept", request); got != want {
		t.Fatalf("same request must have a stable hash: got=%q want=%q", got, want)
	}
	for name, got := range map[string]string{
		"object":  storeOrderActionRequestHash(101, "accept", request),
		"action":  storeOrderActionRequestHash(100, "prepare", request),
		"version": storeOrderActionRequestHash(100, "accept", StoreOrderActionReq{ExpectedVersion: &otherVersion}),
	} {
		if got == want {
			t.Errorf("%s must participate in the idempotency request hash", name)
		}
	}
}

func TestTransitionOrderChecksStatusAndVersionAndIncrementsVersion(t *testing.T) {
	service, db := storeDetailTestService(t)
	row := Order{ID: 9101, OrderNo: "STORE-ACTION-9101", CustomerID: 1, MerchantID: 2, ShopID: 3, Status: "paid", Version: 0}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		updated, err := service.repo.TransitionOrder(context.Background(), tx, row.ID, "paid", 0, map[string]any{"status": "accepted"})
		if err != nil {
			return err
		}
		if !updated {
			t.Fatal("matching status/version must update exactly one row")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var current Order
	if err := db.First(&current, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Status != "accepted" || current.Version != 1 {
		t.Fatalf("unexpected transitioned order: %+v", current)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		updated, err := service.repo.TransitionOrder(context.Background(), tx, row.ID, "accepted", 0, map[string]any{"status": "preparing"})
		if err != nil {
			return err
		}
		if updated {
			t.Fatal("a stale version must not update the order")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&current, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Status != "accepted" || current.Version != 1 {
		t.Fatalf("stale transition changed the order: %+v", current)
	}
}

func TestRejectedStoreOrderActionWritesIndependentSafeFailureAudit(t *testing.T) {
	service, db := storeDetailTestService(t)
	if err := db.AutoMigrate(&AuditLog{}); err != nil {
		t.Fatal(err)
	}
	claims := &auth.Claims{
		AccountType: "merchant", MerchantUserID: "77", MerchantID: "2",
		AuthorizedShopIDs: []string{"3"},
	}
	version := uint(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.AcceptOrder(ctx, claims, http.MethodPost, "/api/v1/store/orders/:id/accept", "store-action-audit", "9101", StoreOrderActionReq{ExpectedVersion: &version})
	if detail := problem.FromError(err); detail.Status != http.StatusForbidden || detail.ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("unexpected rejected action error: %#v", detail)
	}

	var audit AuditLog
	if err := db.Where("action = ? AND resource_id = ?", "store.order.accept", 9101).First(&audit).Error; err != nil {
		t.Fatalf("failure audit must survive a cancelled request context: %v", err)
	}
	if audit.Result != "failed" || audit.ActorType != "merchant" || audit.ActorID != 77 {
		t.Fatalf("unexpected failure audit identity/result: %+v", audit)
	}
	var safe map[string]any
	if err := json.Unmarshal(audit.AfterData, &safe); err != nil {
		t.Fatal(err)
	}
	if len(safe) != 2 || safe["error_code"] != "PERM_FORBIDDEN" || safe["route"] != "POST /api/v1/store/orders/:id/accept" {
		t.Fatalf("failure audit must contain only safe coarse metadata: %#v", safe)
	}
}

func TestIdempotencyRejectionWritesFailureAuditOutsideBusinessTransaction(t *testing.T) {
	service, db := storeDetailTestService(t)
	if err := db.AutoMigrate(&AuditLog{}); err != nil {
		t.Fatal(err)
	}
	claims := &auth.Claims{
		AccountType: "merchant", MerchantUserID: "77", MerchantID: "2",
		AuthorizedShopIDs: []string{"3"}, Permissions: []string{"store_order:accept"},
	}
	version := uint(0)
	_, err := service.AcceptOrder(context.Background(), claims, http.MethodPost, "/api/v1/store/orders/:id/accept", "", "9102", StoreOrderActionReq{ExpectedVersion: &version})
	if detail := problem.FromError(err); detail.Status != http.StatusBadRequest || detail.ErrorCode != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("unexpected idempotency rejection: %#v", detail)
	}
	var audit AuditLog
	if err := db.Where("action = ? AND resource_id = ?", "store.order.accept", 9102).First(&audit).Error; err != nil {
		t.Fatalf("idempotency rejection must be audited independently: %v", err)
	}
	if audit.Result != "failed" || strings.Contains(string(audit.AfterData), "expected_version") || !strings.Contains(string(audit.AfterData), "IDEMPOTENCY_KEY_REQUIRED") {
		t.Fatalf("unexpected idempotency failure audit: %+v", audit)
	}
}

func TestStoreOrderActionResponseTypeHasNoInternalSubjectIDs(t *testing.T) {
	payload, err := json.Marshal(StoreOrderDetailDTO{ID: "9101", ShopID: "3", Status: "accepted", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(payload)
	for _, forbidden := range []string{`"customer_id"`, `"merchant_id"`} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("merchant action response leaked %s: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(encoded, `"status":"accepted"`) || !strings.Contains(encoded, `"version":1`) {
		t.Fatalf("merchant action response must carry post-action status/version: %s", encoded)
	}
}
