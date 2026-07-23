package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/compliance"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestCP1ClosureAcceptanceIntegration 验证CP 1 Closure 验收集成的预期行为。
// TestCP1ClosureAcceptanceIntegration is a database-backed phase-one scenario:
// compliance precheck -> order/payment -> print -> provisioning -> assign and
// reassign -> pickup/delivery verification -> inbox -> immediate account revoke.
func TestCP1ClosureAcceptanceIntegration(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run CP1 acceptance")
	}
	ctx := context.Background()
	cfg := config.Load()
	cfg.CP1.PrintEnabled = true
	cfg.CP1.NotificationEnabled = false // inbox remains mandatory; external side effects stay off.
	cfg.CP1.ProvisioningEnabled = true
	cfg.CP1.ForceActionEnabled = true
	cfg.CP1.PickupVerificationMode = "enforce"
	cfg.CP1.DeliveryVerificationMode = "enforce"
	cfg.CP1.ComplianceMode = "enforce"
	cfg.CP1.IdentityProvider = "fake"
	cfg.CP1.WorkerBatchSize = 1000
	cfg.RiderApplication.Enabled = true
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("UPDATE print_settings SET enabled=1, version=version+1 WHERE shop_id=4201").Error; err != nil {
		t.Fatal(err)
	}

	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 15})
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	defer redisClient.Close()
	defer redisClient.FlushDB(ctx)
	router := NewRouter(Dependencies{Config: cfg, Log: log, DB: tx, Redis: redisClient})

	phone := fmt.Sprintf("137%08d", time.Now().UnixNano()%100000000)
	performOK(t, router, http.MethodPost, "/api/v1/auth/customer/send-code", "", "", map[string]any{"phone": phone})
	login := performOK(t, router, http.MethodPost, "/api/v1/auth/customer/sms-login", "", "", map[string]any{"phone": phone, "code": "123456"})
	customerToken := stringValue(t, object(t, login["data"])["access_token"])
	address := performOK(t, router, http.MethodPost, "/api/v1/addresses", customerToken, "cp1-address-0001", map[string]any{
		"contact_name": "一期验收用户", "contact_phone": phone,
		"province": "广东省", "city": "深圳市", "city_code": "440300", "district": "南山区", "district_code": "440305",
		"address_detail": "一期验收地址 1 号", "location_source": "map_pin",
		"latitude": 22.54, "longitude": 113.93, "coordinate_system": "gcj02", "is_default": true,
	})
	addressID := stringValue(t, object(t, address["data"])["id"])
	orderBody := map[string]any{"shop_id": "4201", "address_id": addressID, "items": []map[string]any{{"shop_product_id": "8001", "quantity": 1}}}
	var beforeOrders int64
	tx.Table("orders").Count(&beforeOrders)
	status, rejected := perform(t, router, http.MethodPost, "/api/v1/orders", customerToken, "cp1-unverified-order", orderBody)
	if status != http.StatusUnprocessableEntity || rejected["error_code"] != "REALNAME_REQUIRED" {
		t.Fatalf("unverified alcohol order was not rejected safely: status=%d body=%#v", status, rejected)
	}
	var afterRejected int64
	tx.Table("orders").Count(&afterRejected)
	if afterRejected != beforeOrders {
		t.Fatalf("compliance rejection created an order: before=%d after=%d", beforeOrders, afterRejected)
	}

	identityStatus, identity := perform(t, router, http.MethodPost, "/api/v1/identity-verifications", customerToken, "cp1-identity-001", map[string]any{
		"purpose": "alcohol_purchase", "verification_level": "identity_and_liveness", "consent_version": "privacy-2026-07",
	})
	if identityStatus != http.StatusAccepted {
		t.Fatalf("identity session status=%d body=%#v", identityStatus, identity)
	}
	identityData := object(t, identity["data"])
	identityAgainStatus, identityAgain := perform(t, router, http.MethodPost, "/api/v1/identity-verifications", customerToken, "cp1-identity-001", map[string]any{
		"purpose": "alcohol_purchase", "verification_level": "identity_and_liveness", "consent_version": "privacy-2026-07",
	})
	if identityAgainStatus != http.StatusAccepted {
		t.Fatalf("idempotent identity session status=%d body=%#v", identityAgainStatus, identityAgain)
	}
	identityAgainData := object(t, identityAgain["data"])
	if identityAgainData["verification_id"] != identityData["verification_id"] || identityAgainData["session_url"] != identityData["session_url"] {
		t.Fatalf("idempotent identity session changed response: first=%#v second=%#v", identityData, identityAgainData)
	}
	var beforePendingOrder int64
	tx.Table("orders").Count(&beforePendingOrder)
	status, rejected = perform(t, router, http.MethodPost, "/api/v1/orders", customerToken, "cp1-pending-identity-order", orderBody)
	if status != http.StatusConflict || rejected["error_code"] != "IDENTITY_VERIFICATION_PENDING" {
		t.Fatalf("pending verification did not block a new alcohol order: status=%d body=%#v", status, rejected)
	}
	var afterPendingOrder int64
	tx.Table("orders").Count(&afterPendingOrder)
	if beforePendingOrder != afterPendingOrder {
		t.Fatalf("pending compliance rejection created an order: before=%d after=%d", beforePendingOrder, afterPendingOrder)
	}
	verificationID := stringValue(t, identityData["verification_id"])
	if err := tx.Table("identity_verification_requests").Where("id=?", verificationID).Update("session_expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	status, rejected = perform(t, router, http.MethodPost, "/api/v1/orders", customerToken, "cp1-expired-identity-session-order", orderBody)
	if status != http.StatusUnprocessableEntity || rejected["error_code"] != "REALNAME_REQUIRED" {
		t.Fatalf("expired identity session did not require a new verification: status=%d body=%#v", status, rejected)
	}
	if err := tx.Table("identity_verification_requests").Where("id=?", verificationID).Update("session_expires_at", time.Now().Add(15*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	sessionURL, err := url.Parse(stringValue(t, identityData["session_url"]))
	if err != nil {
		t.Fatal(err)
	}
	callbackPayload, _ := json.Marshal(map[string]any{
		"event_id": "fake-identity-event-001", "provider_request_id": path.Base(sessionURL.Path), "state": sessionURL.Query().Get("state"),
		"status": "verified", "adult_result": "adult", "provider_subject_id": "fake-adult-subject",
		"verification_level": "identity_and_liveness", "result_reference": "fake-result-001",
	})
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	invalidCallbackReq := httptest.NewRequest(http.MethodPost, "/api/v1/identity-verifications/fake/callbacks", bytes.NewReader(callbackPayload))
	invalidCallbackReq.Header.Set("Content-Type", "application/json")
	invalidCallbackReq.Header.Set(compliance.IdentityTimestampHeader, timestamp)
	invalidCallbackReq.Header.Set(compliance.IdentitySignatureHeader, "00")
	invalidCallbackRecorder := httptest.NewRecorder()
	router.ServeHTTP(invalidCallbackRecorder, invalidCallbackReq)
	if invalidCallbackRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("forged identity callback status=%d body=%s", invalidCallbackRecorder.Code, invalidCallbackRecorder.Body.String())
	}
	callbackReq := httptest.NewRequest(http.MethodPost, "/api/v1/identity-verifications/fake/callbacks", bytes.NewReader(callbackPayload))
	callbackReq.Header.Set("Content-Type", "application/json")
	callbackReq.Header.Set(compliance.IdentityTimestampHeader, timestamp)
	callbackReq.Header.Set(compliance.IdentitySignatureHeader, compliance.SignFakeCallback(cfg.CP1.IdentityCallbackSecret, timestamp, callbackPayload))
	callbackRecorder := httptest.NewRecorder()
	router.ServeHTTP(callbackRecorder, callbackReq)
	if callbackRecorder.Code != http.StatusOK {
		t.Fatalf("identity callback status=%d body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	duplicateCallbackReq := httptest.NewRequest(http.MethodPost, "/api/v1/identity-verifications/fake/callbacks", bytes.NewReader(callbackPayload))
	duplicateCallbackReq.Header.Set("Content-Type", "application/json")
	duplicateCallbackReq.Header.Set(compliance.IdentityTimestampHeader, timestamp)
	duplicateCallbackReq.Header.Set(compliance.IdentitySignatureHeader, compliance.SignFakeCallback(cfg.CP1.IdentityCallbackSecret, timestamp, callbackPayload))
	duplicateCallbackRecorder := httptest.NewRecorder()
	router.ServeHTTP(duplicateCallbackRecorder, duplicateCallbackReq)
	if duplicateCallbackRecorder.Code != http.StatusOK {
		t.Fatalf("duplicate identity callback status=%d body=%s", duplicateCallbackRecorder.Code, duplicateCallbackRecorder.Body.String())
	}
	var callbackRows int64
	tx.Table("identity_verification_callbacks").Where("provider=? AND provider_event_id=?", "fake", "fake-identity-event-001").Count(&callbackRows)
	if callbackRows != 1 {
		t.Fatalf("duplicate callback created %d callback rows", callbackRows)
	}
	var verifiedEvents int64
	tx.Table("outbox_events").Where("event_type=? AND aggregate_type=? AND aggregate_id=?", "identity.verification.updated", "identity_verification", identityData["verification_id"]).Count(&verifiedEvents)
	if verifiedEvents != 1 {
		t.Fatalf("verified callback created %d RabbitMQ outbox events", verifiedEvents)
	}
	identity = performOK(t, router, http.MethodGet, "/api/v1/identity-verifications/me", customerToken, "", nil)
	identityData = object(t, identity["data"])
	if identityData["adult_result"] != "adult" || identityData["status"] != "verified" {
		t.Fatalf("identity result was not verified adult: %#v", identityData)
	}
	var storedIdentity compliance.Request
	if err := tx.First(&storedIdentity, stringValue(t, identityData["verification_id"])).Error; err != nil {
		t.Fatal(err)
	}
	if storedIdentity.DocumentHash != "" || storedIdentity.NameHash != "" || storedIdentity.MaskedName != "" || storedIdentity.MaskedDocumentNo != "" || storedIdentity.BirthDate != nil {
		t.Fatal("identity session persisted prohibited identity material")
	}
	var currentIdentity compliance.Realname
	if err := tx.Where("customer_id=?", storedIdentity.CustomerID).First(&currentIdentity).Error; err != nil {
		t.Fatal(err)
	}
	if currentIdentity.ExpiresAt != nil {
		t.Fatal("adult verification received an unintended fixed expiry")
	}
	gateCases := []struct {
		name        string
		status      string
		adultResult string
		expiresAt   any
		revokedAt   any
		errorCode   string
	}{
		{name: "minor", status: compliance.StatusVerified, adultResult: compliance.AdultMinor, errorCode: "UNDERAGE_RESTRICTED"},
		{name: "unknown", status: compliance.StatusVerified, adultResult: compliance.AdultUnknown, errorCode: "REALNAME_REQUIRED"},
		{name: "expired", status: compliance.StatusVerified, adultResult: compliance.AdultAdult, expiresAt: time.Now().Add(-time.Minute), errorCode: "REALNAME_REQUIRED"},
		{name: "revoked", status: compliance.StatusRevoked, adultResult: compliance.AdultAdult, revokedAt: time.Now(), errorCode: "REALNAME_REQUIRED"},
	}
	for _, gateCase := range gateCases {
		if err := tx.Table("customer_realname_verifications").Where("customer_id=?", storedIdentity.CustomerID).Updates(map[string]any{
			"status": gateCase.status, "adult_result": gateCase.adultResult, "expires_at": gateCase.expiresAt, "revoked_at": gateCase.revokedAt,
		}).Error; err != nil {
			t.Fatal(err)
		}
		status, rejected = perform(t, router, http.MethodPost, "/api/v1/orders", customerToken, "cp1-identity-gate-"+gateCase.name, orderBody)
		if status != http.StatusUnprocessableEntity || rejected["error_code"] != gateCase.errorCode {
			t.Fatalf("%s identity gate status=%d body=%#v", gateCase.name, status, rejected)
		}
	}
	if err := tx.Table("customer_realname_verifications").Where("customer_id=?", storedIdentity.CustomerID).Updates(map[string]any{
		"status": compliance.StatusVerified, "adult_result": compliance.AdultAdult, "expires_at": nil, "revoked_at": nil, "revoked_reason": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}

	created := performOK(t, router, http.MethodPost, "/api/v1/orders", customerToken, "cp1-order-000001", orderBody)
	orderID := stringValue(t, object(t, created["data"])["order_id"])
	revokedPayload, _ := json.Marshal(map[string]any{
		"event_id": "fake-identity-event-002", "provider_request_id": path.Base(sessionURL.Path), "state": sessionURL.Query().Get("state"),
		"status": "revoked", "adult_result": "adult", "provider_subject_id": "fake-adult-subject",
		"verification_level": "identity_and_liveness", "result_reference": "fake-result-002",
	})
	revokedTimestamp := strconv.FormatInt(time.Now().Unix(), 10)
	revokedReq := httptest.NewRequest(http.MethodPost, "/api/v1/identity-verifications/fake/callbacks", bytes.NewReader(revokedPayload))
	revokedReq.Header.Set("Content-Type", "application/json")
	revokedReq.Header.Set(compliance.IdentityTimestampHeader, revokedTimestamp)
	revokedReq.Header.Set(compliance.IdentitySignatureHeader, compliance.SignFakeCallback(cfg.CP1.IdentityCallbackSecret, revokedTimestamp, revokedPayload))
	revokedRecorder := httptest.NewRecorder()
	router.ServeHTTP(revokedRecorder, revokedReq)
	if revokedRecorder.Code != http.StatusOK {
		t.Fatalf("revoked identity callback status=%d body=%s", revokedRecorder.Code, revokedRecorder.Body.String())
	}
	identity = performOK(t, router, http.MethodGet, "/api/v1/identity-verifications/me", customerToken, "", nil)
	if object(t, identity["data"])["status"] != "revoked" {
		t.Fatalf("revoked identity result was not persisted: %#v", identity)
	}
	var revokedEvents int64
	tx.Table("outbox_events").Where("event_type=? AND aggregate_type=? AND aggregate_id=?", "identity.verification.revoked", "identity_verification", storedIdentity.ID).Count(&revokedEvents)
	if revokedEvents != 1 {
		t.Fatalf("revoked callback created %d RabbitMQ outbox events", revokedEvents)
	}
	var beforeRevokedOrder int64
	tx.Table("orders").Count(&beforeRevokedOrder)
	status, rejected = perform(t, router, http.MethodPost, "/api/v1/orders", customerToken, "cp1-revoked-order", orderBody)
	if status != http.StatusUnprocessableEntity || rejected["error_code"] != "REALNAME_REQUIRED" {
		t.Fatalf("revoked verification did not block a new alcohol order: status=%d body=%#v", status, rejected)
	}
	var afterRevokedOrder int64
	tx.Table("orders").Count(&afterRevokedOrder)
	if beforeRevokedOrder != afterRevokedOrder {
		t.Fatalf("revoked compliance rejection created an order: before=%d after=%d", beforeRevokedOrder, afterRevokedOrder)
	}
	performOK(t, router, http.MethodPost, "/api/v1/orders/"+orderID+"/pay/mock", customerToken, "cp1-payment-001", map[string]any{"channel": "mock"})

	merchantLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/merchant/login", "", "", map[string]any{"username": "merchant_demo", "password": "merchant123"})
	merchantToken := stringValue(t, object(t, merchantLogin["data"])["access_token"])
	performStoreOrderAction(t, router, tx, orderID, "accept", merchantToken, "cp1-store-accept")
	performStoreOrderAction(t, router, tx, orderID, "start-preparing", merchantToken, "cp1-store-start")
	performStoreOrderAction(t, router, tx, orderID, "prepare", merchantToken, "cp1-store-prepare")
	var firstPrints int64
	tx.Table("print_tasks").Where("order_id=? AND event_type='order_accepted' AND reprint_seq=0", orderID).Count(&firstPrints)
	if firstPrints != 1 {
		t.Fatalf("expected one first print task, got %d", firstPrints)
	}
	printjob.NewWorker(cfg.CP1, tx, snowflake.New(997), &printjob.FakeProvider{}, "cp1-print", log).RunOnce(ctx)
	var succeededPrints int64
	tx.Table("print_tasks").Where("order_id=? AND status='succeeded'", orderID).Count(&succeededPrints)
	if succeededPrints < 1 {
		t.Fatal("fake print provider did not complete a task")
	}
	var reclaimTask struct {
		ID       uint64
		Attempts uint
	}
	if err := tx.Table("print_tasks").Select("id,attempts").Where("order_id=?", orderID).Order("id").First(&reclaimTask).Error; err != nil {
		t.Fatal(err)
	}
	expiredLease := time.Now().Add(-time.Minute)
	if err := tx.Table("print_tasks").Where("id=?", reclaimTask.ID).Updates(map[string]any{"status": "processing", "locked_by": "crashed-worker", "locked_until": expiredLease, "succeeded_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	printjob.NewWorker(cfg.CP1, tx, snowflake.New(993), &printjob.FakeProvider{}, "cp1-print-recovery", log).RunOnce(ctx)
	var reclaimedPrints int64
	tx.Table("print_tasks").Where("id=? AND status='succeeded' AND attempts=?", reclaimTask.ID, reclaimTask.Attempts+1).Count(&reclaimedPrints)
	if reclaimedPrints != 1 {
		t.Fatal("expired print worker lease was not reclaimed")
	}

	adminLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/admin/login", "", "", map[string]any{"username": "admin", "password": "admin123"})
	adminToken := stringValue(t, object(t, adminLogin["data"])["access_token"])
	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	provisionBody := map[string]any{
		"merchant": map[string]any{"code": "cp1-" + runID, "name": "一期原子开通商户"},
		"shop":     map[string]any{"name": "一期原子开通门店", "city": "深圳市", "district": "南山区", "address": "验收路 1 号"},
		"account":  map[string]any{"username": "cp1_merchant_" + runID}, "merchant_user_name": "一期管理员",
	}
	provisioned := performOK(t, router, http.MethodPost, "/api/v1/admin/merchants/provision", adminToken, "cp1-provision-"+runID, provisionBody)
	provisionedAgain := performOK(t, router, http.MethodPost, "/api/v1/admin/merchants/provision", adminToken, "cp1-provision-"+runID, provisionBody)
	if stringValue(t, object(t, provisioned["data"])["id"]) != stringValue(t, object(t, provisionedAgain["data"])["id"]) {
		t.Fatal("provisioning idempotency returned a different operation")
	}

	adminCreatedPhone := "135" + runID[len(runID)-8:]
	adminCreated := performOK(t, router, http.MethodPost, "/api/v1/admin/riders", adminToken, "cp1-admin-rider-create-"+runID, map[string]any{
		"name": "一期后台创建骑手", "phone": adminCreatedPhone,
		"service_scope": map[string]any{"shop_ids": []string{"4201"}},
	})
	adminCreatedRiderID := stringValue(t, object(t, adminCreated["data"])["id"])
	adminCreatedAccountID := stringValue(t, object(t, adminCreated["data"])["account_id"])
	if status, body := perform(t, router, http.MethodPost, "/api/v1/admin/accounts/"+adminCreatedAccountID+"/reset-password", adminToken, "cp1-admin-rider-reset-password-"+runID, map[string]any{
		"reason": "验证骑手不支持密码",
	}); status != http.StatusConflict || body["error_code"] != "ACCOUNT_PASSWORD_UNSUPPORTED" {
		t.Fatalf("rider password reset status=%d body=%#v", status, body)
	}
	performOK(t, router, http.MethodPost, "/api/v1/auth/rider/send-code", "", "", map[string]any{"phone": adminCreatedPhone})
	if status, _ := perform(t, router, http.MethodPost, "/api/v1/auth/rider/sms-login", "", "", map[string]any{"phone": adminCreatedPhone, "code": "123456"}); status != http.StatusForbidden {
		t.Fatalf("pending admin-created rider login status=%d, want %d", status, http.StatusForbidden)
	}
	if status, _ := perform(t, router, http.MethodPatch, "/api/v1/admin/riders/"+adminCreatedRiderID+"/status", adminToken, "cp1-admin-rider-premature-activate-"+runID, map[string]any{
		"status": "active", "reason": "验证审核不可绕过",
	}); status != http.StatusConflict {
		t.Fatalf("pending admin-created rider activation status=%d, want %d", status, http.StatusConflict)
	}
	performOK(t, router, http.MethodPost, "/api/v1/admin/riders/"+adminCreatedRiderID+"/review", adminToken, "cp1-admin-rider-review-"+runID, map[string]any{
		"decision": "approved", "reason": "一期后台创建验收",
	})
	// The failed pre-review login consumed its OTP. A post-review login must use
	// a newly issued code instead of replaying the previous credential. Expire
	// the test cooldown to model the normal 60-second resend interval.
	if err := redisClient.Del(ctx, "rate:sms:login:rider:cooldown:"+adminCreatedPhone).Err(); err != nil {
		t.Fatal(err)
	}
	performOK(t, router, http.MethodPost, "/api/v1/auth/rider/send-code", "", "", map[string]any{"phone": adminCreatedPhone})
	adminCreatedRiderLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/rider/sms-login", "", "", map[string]any{"phone": adminCreatedPhone, "code": "123456"})
	adminCreatedRiderToken := stringValue(t, object(t, adminCreatedRiderLogin["data"])["access_token"])

	riderApplicationPhone := "136" + runID[len(runID)-8:]
	performOK(t, router, http.MethodPost, "/api/v1/auth/rider/send-code", "", "", map[string]any{"phone": riderApplicationPhone})
	riderApplicationStatus, riderApplication := perform(t, router, http.MethodPost, "/api/v1/rider-applications", "", "cp1-rider-application-"+runID, map[string]any{
		"name": "一期改派骑手", "phone": riderApplicationPhone, "code": "123456",
		"service_scope": map[string]any{"shop_ids": []string{"4201"}},
	})
	if riderApplicationStatus != http.StatusCreated {
		t.Fatalf("rider application status=%d body=%#v", riderApplicationStatus, riderApplication)
	}
	riderApplicationID := stringValue(t, object(t, riderApplication["data"])["id"])
	riderReviewed := performOK(t, router, http.MethodPost, "/api/v1/admin/rider-applications/"+riderApplicationID+"/review", adminToken, "cp1-rider-review-"+runID, map[string]any{
		"decision": "approved", "reason": "一期集成验收", "expected_version": 1,
	})
	rider2ID := stringValue(t, object(t, riderReviewed["data"])["rider_id"])

	// Application submission also consumes the OTP atomically, so formal rider
	// login requires a fresh one-time code after approval.
	if err := redisClient.Del(ctx, "rate:sms:login:rider:cooldown:"+riderApplicationPhone).Err(); err != nil {
		t.Fatal(err)
	}
	performOK(t, router, http.MethodPost, "/api/v1/auth/rider/send-code", "", "", map[string]any{"phone": riderApplicationPhone})
	riderLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/rider/sms-login", "", "", map[string]any{"phone": riderApplicationPhone, "code": "123456"})
	riderToken := stringValue(t, object(t, riderLogin["data"])["access_token"])
	performOK(t, router, http.MethodPost, "/api/v1/delivery/riders/me/heartbeat", riderToken, "", map[string]any{
		"device_id": "cp1-rider-device", "sequence": uint64(time.Now().UnixNano()), "captured_at": time.Now().Format(time.RFC3339Nano),
		"latitude": 22.541, "longitude": 113.931, "coordinate_system": "gcj02", "accuracy_m": 20,
	})
	performOK(t, router, http.MethodPut, "/api/v1/delivery/riders/me/work-status", riderToken, "cp1-rider-online-"+runID, map[string]any{"status": "online", "expected_version": 1})
	var deliveryID string
	if err := tx.Table("delivery_orders").Select("id").Where("order_id=?", orderID).Scan(&deliveryID).Error; err != nil || deliveryID == "" {
		t.Fatalf("find paid delivery for manual assignment: id=%s err=%v", deliveryID, err)
	}
	assigned := performOK(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+deliveryID+"/assign", adminToken, "cp1-assign-"+runID, map[string]any{"rider_id": rider2ID, "reason_code": "MANUAL_DISPATCH", "reason": "一期验收初派", "expected_version": 1})
	assignedVersion := uint(object(t, assigned["data"])["assignment_version"].(float64))
	oldPickup := stringValue(t, object(t, performOK(t, router, http.MethodGet, "/api/v1/store/orders/"+orderID+"/verification", merchantToken, "", nil)["data"])["code"])
	performOK(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+deliveryID+"/reassign", adminToken, "cp1-reassign-"+runID, map[string]any{"rider_id": adminCreatedRiderID, "reason_code": "RIDER_SWITCH", "reason": "一期验收改派", "expected_version": assignedVersion})
	newPickup := stringValue(t, object(t, performOK(t, router, http.MethodGet, "/api/v1/store/orders/"+orderID+"/verification", merchantToken, "", nil)["data"])["code"])
	if oldPickup == newPickup {
		t.Fatal("reassignment did not invalidate and regenerate the pickup code")
	}

	status, _ = perform(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/pickup", adminCreatedRiderToken, "cp1-wrong-pickup", map[string]any{"pickup_code": "000000"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("wrong pickup code status=%d", status)
	}
	performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/pickup", adminCreatedRiderToken, "cp1-correct-pickup", map[string]any{"pickup_code": newPickup})
	deliveryCode := stringValue(t, object(t, performOK(t, router, http.MethodGet, "/api/v1/orders/"+orderID+"/verification", customerToken, "", nil)["data"])["code"])
	performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/complete", adminCreatedRiderToken, "cp1-correct-delivery", map[string]any{"delivery_code": deliveryCode})

	// A separate delivery proves the full force-complete authorization split:
	// operation can only request, admin_manager can only approve, both permissions
	// are rechecked from the database even when an old token still contains them,
	// and an unrelated approver cannot execute the named checker's approval.
	forceAdminIDs := snowflake.New(995)
	makerAccountID, makerAdminID := forceAdminIDs.Next(), forceAdminIDs.Next()
	checkerAccountID, checkerAdminID := forceAdminIDs.Next(), forceAdminIDs.Next()
	forcePassword := "cp1-force-admin-strong-password"
	forceHash, err := bcrypt.GenerateFromPassword([]byte(forcePassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	makerUsername := "cp1_operation_" + runID
	checkerUsername := "cp1_checker_" + runID
	if err := tx.Exec("INSERT INTO accounts (id,account_type,username,password_hash,status) VALUES (?,'admin',?,?,'active'),(?,'admin',?,?,'active')", makerAccountID, makerUsername, string(forceHash), checkerAccountID, checkerUsername, string(forceHash)).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec("INSERT INTO admin_users (id,account_id,role_id,admin_sub_role,name,status) VALUES (?,?,1003,'operations','一期发起人','active'),(?,?,1002,'admin_manager','一期复核人','active')", makerAdminID, makerAccountID, checkerAdminID, checkerAccountID).Error; err != nil {
		t.Fatal(err)
	}
	makerLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/admin/login", "", "", map[string]any{"username": makerUsername, "password": forcePassword})
	makerToken := stringValue(t, object(t, makerLogin["data"])["access_token"])
	checkerLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/admin/login", "", "", map[string]any{"username": checkerUsername, "password": forcePassword})
	checkerToken := stringValue(t, object(t, checkerLogin["data"])["access_token"])
	secondBody := map[string]any{"shop_id": "4201", "address_id": addressID, "items": []map[string]any{{"shop_product_id": "8005", "quantity": 1}}}
	secondCreated := performOK(t, router, http.MethodPost, "/api/v1/orders", customerToken, "cp1-force-order", secondBody)
	secondOrderID := stringValue(t, object(t, secondCreated["data"])["order_id"])
	performOK(t, router, http.MethodPost, "/api/v1/orders/"+secondOrderID+"/pay/mock", customerToken, "cp1-force-payment", map[string]any{"channel": "mock"})
	performStoreOrderAction(t, router, tx, secondOrderID, "accept", merchantToken, "cp1-force-accept")
	performStoreOrderAction(t, router, tx, secondOrderID, "start-preparing", merchantToken, "cp1-force-start")
	performStoreOrderAction(t, router, tx, secondOrderID, "prepare", merchantToken, "cp1-force-prepare")
	openOrderGrab(t, cfg, tx, redisClient, log, secondOrderID)
	secondDeliveries := performOK(t, router, http.MethodGet, "/api/v1/delivery/orders?page_size=100", riderToken, "", nil)
	secondDeliveryID := findDeliveryID(t, array(t, object(t, secondDeliveries["data"])["items"]), secondOrderID)
	acceptedSecond := performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+secondDeliveryID+"/accept", riderToken, "cp1-force-rider-accept", map[string]any{"expected_assignment_version": 1})
	forceVersion := uint(object(t, acceptedSecond["data"])["assignment_version"].(float64))
	secondPickup := stringValue(t, object(t, performOK(t, router, http.MethodGet, "/api/v1/store/orders/"+secondOrderID+"/verification", merchantToken, "", nil)["data"])["code"])
	performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+secondDeliveryID+"/pickup", riderToken, "cp1-force-pickup", map[string]any{"pickup_code": secondPickup})
	forceRequestBody := map[string]any{"checker_admin_id": fmt.Sprintf("%d", checkerAdminID), "reason_code": "CUSTOMER_CONFIRMED", "reason": "顾客确认收货且骑手设备故障", "expected_version": forceVersion}
	if status, _ := perform(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+secondDeliveryID+"/force-complete-requests", checkerToken, "cp1-manager-cannot-request", forceRequestBody); status != http.StatusForbidden {
		t.Fatalf("admin_manager requested force completion: status=%d", status)
	}
	if err := tx.Table("role_permissions").Where("role_id=1003 AND permission_id=2143").Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if status, body := perform(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+secondDeliveryID+"/force-complete-requests", makerToken, "cp1-revoked-maker-request", forceRequestBody); status != http.StatusForbidden || body["error_code"] != "PERM_FORBIDDEN" {
		t.Fatalf("revoked operation token requested force completion: status=%d body=%#v", status, body)
	}
	if err := tx.Table("role_permissions").Where("role_id=1003 AND permission_id=2143").Update("deleted_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	approvalResponse := performOK(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+secondDeliveryID+"/force-complete-requests", makerToken, "cp1-force-request", forceRequestBody)
	approvalID := stringValue(t, object(t, approvalResponse["data"])["id"])
	forceApproveBody := map[string]any{"approval_id": approvalID, "expected_version": forceVersion}
	if status, _ := perform(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+secondDeliveryID+"/force-complete", makerToken, "cp1-operation-cannot-approve", forceApproveBody); status != http.StatusForbidden {
		t.Fatalf("operation approved force completion: status=%d", status)
	}
	if status, _ := perform(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+secondDeliveryID+"/force-complete", adminToken, "cp1-unnamed-checker-cannot-approve", forceApproveBody); status != http.StatusForbidden {
		t.Fatalf("unnamed checker executed approval: status=%d", status)
	}
	if err := tx.Table("role_permissions").Where("role_id=1002 AND permission_id=2144").Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if status, body := perform(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+secondDeliveryID+"/force-complete", checkerToken, "cp1-revoked-checker-approve", forceApproveBody); status != http.StatusForbidden || body["error_code"] != "PERM_FORBIDDEN" {
		t.Fatalf("revoked admin_manager token approved force completion: status=%d body=%#v", status, body)
	}
	if err := tx.Table("role_permissions").Where("role_id=1002 AND permission_id=2144").Update("deleted_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	performOK(t, router, http.MethodPost, "/api/v1/admin/deliveries/"+secondDeliveryID+"/force-complete", checkerToken, "cp1-force-checker-approve", forceApproveBody)
	var approvedOverrides int64
	tx.Table("admin_override_approvals").Where("id=? AND maker_admin_id<>checker_admin_id AND status='approved'", approvalID).Count(&approvedOverrides)
	if approvedOverrides != 1 {
		t.Fatal("two-person approval was not durably recorded")
	}

	if err := tx.Table("notification_templates").Where("status='draft'").Update("status", "published").Error; err != nil {
		t.Fatal(err)
	}
	notifyCfg := cfg.CP1
	notifyCfg.NotificationEnabled = true
	notification.NewWorker(notifyCfg, tx, snowflake.New(996), &notification.FakeProvider{}, "cp1-notify", log).RunOnce(ctx)
	var customerID uint64
	tx.Table("orders").Select("customer_id").Where("id=?", orderID).Scan(&customerID)
	var messages int64
	tx.Table("message_inboxes").Where("customer_id=?", customerID).Count(&messages)
	if messages < 5 {
		t.Fatalf("expected phase-one lifecycle inbox messages, got %d", messages)
	}
	var notificationTask struct {
		ID       uint64
		Attempts uint
	}
	if err := tx.Table("notification_deliveries").Select("id,attempts").Where("event_type='order.paid' AND recipient_type='customer'").Order("id DESC").First(&notificationTask).Error; err != nil {
		t.Fatalf("published notification was not delivered: %v", err)
	}
	if err := tx.Table("notification_deliveries").Where("id=?", notificationTask.ID).Updates(map[string]any{"status": "processing", "locked_by": "crashed-worker", "locked_until": expiredLease, "sent_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	notification.NewWorker(notifyCfg, tx, snowflake.New(992), &notification.FakeProvider{}, "cp1-notify-recovery", log).RunOnce(ctx)
	var reclaimedNotifications int64
	tx.Table("notification_deliveries").Where("id=? AND status='succeeded' AND attempts=?", notificationTask.ID, notificationTask.Attempts+1).Count(&reclaimedNotifications)
	if reclaimedNotifications != 1 {
		t.Fatal("expired notification worker lease was not reclaimed")
	}
	var completed int64
	tx.Table("orders").Where("id=? AND status='completed' AND delivery_status='completed'", orderID).Count(&completed)
	if completed != 1 {
		t.Fatal("delivery verification did not atomically complete the order")
	}

	performOK(t, router, http.MethodPatch, "/api/v1/admin/accounts/"+adminCreatedAccountID+"/status", adminToken, "cp1-disable-rider", map[string]any{"status": "disabled", "reason": "一期停用验收"})
	if status, _ := perform(t, router, http.MethodGet, "/api/v1/delivery/orders?page_size=10", adminCreatedRiderToken, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("disabled account retained access: status=%d", status)
	}
}
