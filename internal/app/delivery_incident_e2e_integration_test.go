package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/evidencetoken"
	"jiuxiaoer-admin/backend-go/internal/pkg/evidenceview"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type deliveryIncidentE2EFixture struct {
	ctx                context.Context
	cfg                config.Config
	db                 *gorm.DB
	redis              *goredis.Client
	router             http.Handler
	ids                *snowflake.Generator
	riderID            uint64
	shopID             uint64
	otherShopID        uint64
	merchantID         uint64
	adminID            uint64
	checkerAdminID     uint64
	shopProductID      uint64
	productID          uint64
	riderToken         string
	merchantToken      string
	otherMerchantToken string
	adminToken         string
	checkerToken       string
	close              func()
}

type deliveryIncidentFulfillment struct {
	OrderID    uint64
	DeliveryID uint64
	ItemID     uint64
}

func TestDeliveryIncidentHTTPAndNaturalClosureEndToEnd(t *testing.T) {
	fixture := newDeliveryIncidentE2EFixture(t)
	defer fixture.close()

	t.Run("all incident APIs and authorization boundaries", fixture.testIncidentAPIs)
	t.Run("pickup complete force-complete and cancel close incidents atomically", fixture.testNaturalClosureEntrypoints)
	t.Run("natural close failure rolls the fulfillment action back", fixture.testNaturalClosureFailureRollback)
}

func newDeliveryIncidentE2EFixture(t *testing.T) *deliveryIncidentE2EFixture {
	t.Helper()
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run delivery incident HTTP E2E acceptance")
	}
	ctx := context.Background()
	cfg := config.Load()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	db = db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	sqlDB, _ := db.DB()
	tx := db.Begin()
	if tx.Error != nil {
		_ = sqlDB.Close()
		t.Fatalf("begin fixture transaction: %v", tx.Error)
	}
	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 14})
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		_ = tx.Rollback().Error
		_ = sqlDB.Close()
		t.Fatalf("flush isolated redis db: %v", err)
	}

	fixture := &deliveryIncidentE2EFixture{ctx: ctx, cfg: cfg, db: tx, redis: redisClient, ids: snowflake.New(982)}
	fixture.close = func() {
		_ = tx.Rollback().Error
		_ = redisClient.FlushDB(ctx).Err()
		_ = redisClient.Close()
		_ = sqlDB.Close()
	}
	fixture.loadSeedIdentities(t)
	fixture.installAdminPermissions(t)
	fixture.createSecondaryIdentities(t)

	fixture.cfg.DeliveryIncident.Enabled = true
	fixture.cfg.DeliveryIncident.AutoResolveEnabled = true
	fixture.cfg.DeliveryIncident.NotificationEnabled = true
	fixture.cfg.DeliveryIncident.CreateRatePerHour = 1000
	fixture.cfg.DeliveryIncident.EvidenceRatePerHour = 1000
	fixture.cfg.DeliveryIncident.CreateIPRatePerHour = 1000
	fixture.cfg.DeliveryIncident.EvidenceIPRatePerHour = 1000
	fixture.cfg.DeliveryIncident.EvidenceViewBaseURL = "https://media.example.test/private/evidence"
	fixture.cfg.DeliveryIncident.EvidenceViewSecret = "delivery-incident-e2e-view-secret-at-least-32-characters"
	fixture.cfg.DeliveryIncident.EvidenceViewTTL = 5 * time.Minute
	fixture.cfg.DeliveryIncident.RiderAllowlist = []string{strconv.FormatUint(fixture.riderID, 10)}
	fixture.cfg.DeliveryIncident.ShopAllowlist = []string{strconv.FormatUint(fixture.shopID, 10), strconv.FormatUint(fixture.otherShopID, 10)}
	fixture.cfg.CP1.PickupVerificationMode = "off"
	fixture.cfg.CP1.DeliveryVerificationMode = "off"
	fixture.cfg.CP1.ForceActionEnabled = true
	fixture.cfg.Feature.SMSMockEnabled = true
	gin.SetMode(gin.TestMode)
	fixture.router = NewRouter(Dependencies{Config: fixture.cfg, Log: log, DB: tx, Redis: redisClient, IDGen: fixture.ids})

	performOK(t, fixture.router, http.MethodPost, "/api/v1/auth/rider/send-code", "", "", map[string]any{"phone": "13800000003"})
	fixture.riderToken = tokenFromLogin(t, fixture.router, "/api/v1/auth/rider/sms-login", map[string]any{"phone": "13800000003", "code": "123456"})
	fixture.merchantToken = tokenFromLogin(t, fixture.router, "/api/v1/auth/merchant/login", map[string]any{"username": "merchant_demo", "password": "merchant123"})
	fixture.otherMerchantToken = tokenFromLogin(t, fixture.router, "/api/v1/auth/merchant/login", map[string]any{"username": "incident_other_merchant", "password": "merchant123"})
	fixture.adminToken = tokenFromLogin(t, fixture.router, "/api/v1/auth/admin/login", map[string]any{"username": "admin", "password": fixture.cfg.Security.AdminBootstrapPassword})
	fixture.checkerToken = tokenFromLogin(t, fixture.router, "/api/v1/auth/admin/login", map[string]any{"username": "incident_checker", "password": "checker123"})
	return fixture
}

func (f *deliveryIncidentE2EFixture) loadSeedIdentities(t *testing.T) {
	t.Helper()
	var rider struct{ RiderID uint64 }
	if err := f.db.Table("riders r").Select("r.id AS rider_id").Joins("JOIN accounts a ON a.id=r.account_id").Where("a.account_type='rider' AND a.phone='13800000003'").Scan(&rider).Error; err != nil || rider.RiderID == 0 {
		t.Fatalf("seed rider phone 13800000003 is required: id=%d err=%v", rider.RiderID, err)
	}
	f.riderID = rider.RiderID
	var merchant struct {
		ShopID     uint64
		MerchantID uint64
	}
	if err := f.db.Table("merchant_users mu").Select("mus.shop_id,mu.merchant_id").Joins("JOIN accounts a ON a.id=mu.account_id").Joins("JOIN merchant_user_shops mus ON mus.merchant_user_id=mu.id AND mus.deleted_at IS NULL").Where("a.account_type='merchant' AND a.username='merchant_demo'").Order("mus.shop_id").Scan(&merchant).Error; err != nil || merchant.ShopID == 0 {
		t.Fatalf("seed merchant_demo shop is required: %+v err=%v", merchant, err)
	}
	f.shopID, f.merchantID = merchant.ShopID, merchant.MerchantID
	var admin struct {
		AdminID uint64
		RoleID  uint64
	}
	if err := f.db.Table("admin_users au").Select("au.id AS admin_id,au.role_id").Joins("JOIN accounts a ON a.id=au.account_id").Where("a.account_type='admin' AND a.username='admin'").Scan(&admin).Error; err != nil || admin.AdminID == 0 || admin.RoleID == 0 {
		t.Fatalf("seed admin is required: %+v err=%v", admin, err)
	}
	f.adminID = admin.AdminID
	var product struct {
		ShopProductID uint64
		ProductID     uint64
	}
	if err := f.db.Table("shop_products").Select("id AS shop_product_id,product_id").Where("shop_id=? AND deleted_at IS NULL", f.shopID).Order("id").Limit(1).Scan(&product).Error; err != nil || product.ShopProductID == 0 {
		t.Fatalf("seed shop product is required: %+v err=%v", product, err)
	}
	f.shopProductID, f.productID = product.ShopProductID, product.ProductID
}

func (f *deliveryIncidentE2EFixture) installAdminPermissions(t *testing.T) {
	t.Helper()
	var roleID uint64
	if err := f.db.Table("admin_users").Select("role_id").Where("id=?", f.adminID).Scan(&roleID).Error; err != nil || roleID == 0 {
		t.Fatalf("load admin role: id=%d err=%v", roleID, err)
	}
	codes := []string{
		"delivery_incident:list_all", "delivery_incident:view_all", "delivery_incident:acknowledge",
		"delivery_incident:resolve", "delivery_incident:reject", "delivery_incident:audit",
		"order:cancel_all", "delivery:force_complete",
	}
	for index, code := range codes {
		permissionID := uint64(2115 + index)
		if code == "order:cancel_all" {
			permissionID = 2078
		}
		if code == "delivery:force_complete" {
			permissionID = 2068
		}
		parts := strings.SplitN(code, ":", 2)
		if err := f.db.Exec(`INSERT INTO permissions (id,code,resource,action,description,status)
			VALUES (?,?,?,?,?,'active') ON DUPLICATE KEY UPDATE status='active',deleted_at=NULL`, permissionID, code, parts[0], parts[1], "delivery incident E2E").Error; err != nil {
			t.Fatalf("upsert permission %s: %v", code, err)
		}
		if err := f.db.Table("permissions").Select("id").Where("code=?", code).Scan(&permissionID).Error; err != nil || permissionID == 0 {
			t.Fatalf("find permission %s: id=%d err=%v", code, permissionID, err)
		}
		if err := f.db.Exec(`INSERT INTO role_permissions (id,role_id,permission_id)
			VALUES (?,?,?) ON DUPLICATE KEY UPDATE deleted_at=NULL`, f.ids.Next(), roleID, permissionID).Error; err != nil {
			t.Fatalf("grant permission %s: %v", code, err)
		}
	}
}

func (f *deliveryIncidentE2EFixture) createSecondaryIdentities(t *testing.T) {
	t.Helper()
	var roleID uint64
	if err := f.db.Table("admin_users").Select("role_id").Where("id=?", f.adminID).Scan(&roleID).Error; err != nil || roleID == 0 {
		t.Fatalf("load role for checker: %v", err)
	}
	checkerHash, _ := bcrypt.GenerateFromPassword([]byte("checker123"), bcrypt.MinCost)
	checkerAccountID := f.ids.Next()
	f.checkerAdminID = f.ids.Next()
	if err := f.db.Table("accounts").Create(map[string]any{"id": checkerAccountID, "account_type": "admin", "username": "incident_checker", "password_hash": string(checkerHash), "status": "active"}).Error; err != nil {
		t.Fatalf("create checker account: %v", err)
	}
	if err := f.db.Table("admin_users").Create(map[string]any{"id": f.checkerAdminID, "account_id": checkerAccountID, "role_id": roleID, "admin_sub_role": "operations", "name": "Incident checker", "status": "active"}).Error; err != nil {
		t.Fatalf("create checker admin: %v", err)
	}

	merchantHash, _ := bcrypt.GenerateFromPassword([]byte("merchant123"), bcrypt.MinCost)
	otherAccountID, otherMerchantID, otherUserID := f.ids.Next(), f.ids.Next(), f.ids.Next()
	f.otherShopID = f.ids.Next()
	if err := f.db.Table("accounts").Create(map[string]any{"id": otherAccountID, "account_type": "merchant", "username": "incident_other_merchant", "password_hash": string(merchantHash), "status": "active"}).Error; err != nil {
		t.Fatalf("create other merchant account: %v", err)
	}
	if err := f.db.Table("merchants").Create(map[string]any{"id": otherMerchantID, "code": fmt.Sprintf("incident-%d", otherMerchantID), "name": "Other merchant", "status": "active", "review_status": "approved"}).Error; err != nil {
		t.Fatalf("create other merchant: %v", err)
	}
	if err := f.db.Table("merchant_users").Create(map[string]any{"id": otherUserID, "account_id": otherAccountID, "merchant_id": otherMerchantID, "name": "Other merchant user", "status": "active"}).Error; err != nil {
		t.Fatalf("create other merchant user: %v", err)
	}
	if err := f.db.Table("shops").Create(map[string]any{"id": f.otherShopID, "merchant_id": otherMerchantID, "name": "Other shop", "city": "深圳市", "district": "南山区", "address": "E2E other shop", "status": "active", "business_status": "open"}).Error; err != nil {
		t.Fatalf("create other shop: %v", err)
	}
	if err := f.db.Table("merchant_user_shops").Create(map[string]any{"id": f.ids.Next(), "merchant_user_id": otherUserID, "merchant_id": otherMerchantID, "shop_id": f.otherShopID}).Error; err != nil {
		t.Fatalf("authorize other shop: %v", err)
	}
}

func (f *deliveryIncidentE2EFixture) createFulfillment(t *testing.T, status string) deliveryIncidentFulfillment {
	t.Helper()
	row := deliveryIncidentFulfillment{OrderID: f.ids.Next(), DeliveryID: f.ids.Next(), ItemID: f.ids.Next()}
	now := time.Now().UTC()
	orderStatus := "paid"
	if status == "delivering" {
		orderStatus = "delivering"
	}
	order := map[string]any{
		"id": row.OrderID, "order_no": fmt.Sprintf("DIE2E%d", row.OrderID), "customer_id": f.ids.Next(),
		"merchant_id": f.merchantID, "shop_id": f.shopID, "status": orderStatus, "pay_status": "succeeded",
		"delivery_status": status, "goods_amount": 2000, "payable_amount": 2000, "paid_amount": 2000,
		"address_snapshot": datatypes.JSON(`{"contact_phone":"13800000000","longitude":113.93,"latitude":22.54}`), "version": 1,
	}
	if err := f.db.Table("orders").Create(order).Error; err != nil {
		t.Fatalf("create E2E order: %v", err)
	}
	delivery := map[string]any{
		"id": row.DeliveryID, "order_id": row.OrderID, "shop_id": f.shopID, "rider_id": f.riderID,
		"status": status, "assignment_version": 1, "dispatch_status": "assigned", "pickup_ready_status": "ready",
		"accepted_at":        now.Add(-30 * time.Minute),
		"recipient_snapshot": datatypes.JSON(`{"district":"南山区","contact_phone":"13800000000","longitude":113.9305,"latitude":22.5405}`),
	}
	if status == "delivering" {
		delivery["picked_up_at"], delivery["started_at"] = now.Add(-10*time.Minute), now.Add(-10*time.Minute)
	}
	if err := f.db.Table("delivery_orders").Create(delivery).Error; err != nil {
		t.Fatalf("create E2E delivery: %v", err)
	}
	if err := f.db.Table("order_items").Create(map[string]any{
		"id": row.ItemID, "order_id": row.OrderID, "shop_product_id": f.shopProductID, "product_id": f.productID,
		"product_snapshot": datatypes.JSON(`{"name":"E2E bottle","spec":"500ml","cost_price":1,"supplier_phone":"13900000000"}`),
		"quantity":         2, "sale_price_amount": 1000, "total_amount": 2000,
	}).Error; err != nil {
		t.Fatalf("create E2E order item: %v", err)
	}
	return row
}

func (f *deliveryIncidentE2EFixture) createIncident(t *testing.T, fulfillment deliveryIncidentFulfillment, incidentType, reasonCode string) map[string]any {
	t.Helper()
	body := map[string]any{"type": incidentType, "reason_code": reasonCode, "description": "delivery incident end-to-end acceptance"}
	if incidentType == "out_of_stock" || incidentType == "alcohol_damaged" {
		body["items"] = []map[string]any{{"order_item_id": fulfillment.ItemID, "quantity": 1}}
	}
	if incidentType == "customer_unreachable" {
		now := time.Now().UTC()
		body["contact_attempts"] = map[string]any{"count": 2, "first_at": now.Add(-4 * time.Minute), "last_at": now}
	}
	body["location"] = map[string]any{"longitude": 113.9301, "latitude": 22.5401, "accuracy_m": 12.5, "captured_at": time.Now().UTC().Truncate(time.Second)}
	status, response := perform(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/delivery/orders/%d/incidents", fulfillment.DeliveryID), f.riderToken, fmt.Sprintf("incident-create-%d-%s", fulfillment.DeliveryID, incidentType), body)
	if status != http.StatusCreated {
		t.Fatalf("create %s incident: status=%d response=%#v", incidentType, status, response)
	}
	return object(t, response["data"])
}

func (f *deliveryIncidentE2EFixture) testIncidentAPIs(t *testing.T) {
	fulfillment := f.createFulfillment(t, "accepted")
	created := f.createIncident(t, fulfillment, "out_of_stock", "")
	incidentID := stringValue(t, created["id"])
	if created["stage"] != "pickup" || created["status"] != "open" || uintField(t, created, "version") != 1 {
		t.Fatalf("unexpected created incident: %#v", created)
	}
	assertNoIncidentSensitiveFields(t, created)

	status, replay := perform(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/delivery/orders/%d/incidents", fulfillment.DeliveryID), f.riderToken, fmt.Sprintf("incident-create-%d-%s", fulfillment.DeliveryID, "out_of_stock"), map[string]any{
		"type": "out_of_stock", "description": "delivery incident end-to-end acceptance",
		"items":    []map[string]any{{"order_item_id": fulfillment.ItemID, "quantity": 1}},
		"location": map[string]any{"longitude": 113.9301, "latitude": 22.5401, "accuracy_m": 12.5, "captured_at": created["location_captured_at"]},
	})
	if status != http.StatusCreated || object(t, replay["data"])["id"] != incidentID {
		t.Fatalf("idempotent HTTP replay changed the incident: status=%d body=%#v", status, replay)
	}

	riderList := performOK(t, f.router, http.MethodGet, fmt.Sprintf("/api/v1/delivery/orders/%d/incidents", fulfillment.DeliveryID), f.riderToken, "", nil)
	if !containsID(array(t, object(t, riderList["data"])["items"]), "id", incidentID) {
		t.Fatalf("rider list omitted incident %s", incidentID)
	}
	riderDetail := performOK(t, f.router, http.MethodGet, "/api/v1/delivery/incidents/"+incidentID, f.riderToken, "", nil)
	assertNoIncidentSensitiveFields(t, riderDetail["data"])
	storeList := performOK(t, f.router, http.MethodGet, "/api/v1/store/delivery-incidents", f.merchantToken, "", nil)
	if !containsID(array(t, object(t, storeList["data"])["items"]), "id", incidentID) {
		t.Fatalf("store list omitted incident %s", incidentID)
	}
	storeDetail := performOK(t, f.router, http.MethodGet, "/api/v1/store/delivery-incidents/"+incidentID, f.merchantToken, "", nil)
	assertNoIncidentSensitiveFields(t, storeDetail["data"])
	adminList := performOK(t, f.router, http.MethodGet, "/api/v1/admin/delivery-incidents?status=open", f.adminToken, "", nil)
	if !containsID(array(t, object(t, adminList["data"])["items"]), "id", incidentID) {
		t.Fatalf("admin list omitted incident %s", incidentID)
	}
	adminDetail := performOK(t, f.router, http.MethodGet, "/api/v1/admin/delivery-incidents/"+incidentID, f.adminToken, "", nil)
	assertNoIncidentSensitiveFields(t, adminDetail["data"])

	otherList := performOK(t, f.router, http.MethodGet, "/api/v1/store/delivery-incidents", f.otherMerchantToken, "", nil)
	if containsID(array(t, object(t, otherList["data"])["items"]), "id", incidentID) {
		t.Fatal("other shop saw an unauthorized incident in its list")
	}
	otherStatus, otherDetail := perform(t, f.router, http.MethodGet, "/api/v1/store/delivery-incidents/"+incidentID, f.otherMerchantToken, "", nil)
	expectProblem(t, otherStatus, otherDetail, http.StatusNotFound, "DELIVERY_INCIDENT_NOT_FOUND")

	ack := performOK(t, f.router, http.MethodPost, "/api/v1/admin/delivery-incidents/"+incidentID+"/acknowledge", f.adminToken, "incident-ack-"+incidentID, map[string]any{"expected_version": 1, "note": "operations checking"})
	ackData := object(t, ack["data"])
	if ackData["status"] != "acknowledged" || uintField(t, ackData, "version") != 2 {
		t.Fatalf("acknowledge transition mismatch: %#v", ackData)
	}
	invalidReturnStatus, invalidReturn := perform(t, f.router, http.MethodPost, "/api/v1/admin/delivery-incidents/"+incidentID+"/resolve", f.adminToken, "incident-resolve-returned-"+incidentID, map[string]any{"expected_version": 2, "resolution_code": "returned_to_store", "resolution_note": "store confirmed"})
	expectProblem(t, invalidReturnStatus, invalidReturn, http.StatusConflict, "INVALID_RETURN_STATE")

	resolved := performOK(t, f.router, http.MethodPost, "/api/v1/admin/delivery-incidents/"+incidentID+"/resolve", f.adminToken, "incident-resolve-other-"+incidentID, map[string]any{"expected_version": 2, "resolution_code": "other", "resolution_note": "closed after operations review"})
	resolvedData := object(t, resolved["data"])
	if resolvedData["status"] != "resolved" || uintField(t, resolvedData, "version") != 3 {
		t.Fatalf("resolve transition mismatch: %#v", resolvedData)
	}

	damagedFulfillment := f.createFulfillment(t, "accepted")
	damaged := f.createIncident(t, damagedFulfillment, "alcohol_damaged", "")
	if damaged["status"] != "evidence_required" {
		t.Fatalf("damaged incident without evidence must require evidence: %#v", damaged)
	}
	damagedID := stringValue(t, damaged["id"])
	evidenceToken := f.evidenceToken(t, f.ids.Next())
	withEvidence := performOK(t, f.router, http.MethodPost, "/api/v1/delivery/incidents/"+damagedID+"/evidence", f.riderToken, "incident-evidence-"+damagedID, map[string]any{"expected_version": 1, "evidence_tokens": []string{evidenceToken}})
	evidenceData := object(t, withEvidence["data"])
	if evidenceData["status"] != "open" || uintField(t, evidenceData, "version") != 2 || len(array(t, evidenceData["evidence"])) != 1 {
		t.Fatalf("evidence transition mismatch: %#v", evidenceData)
	}
	assertNoIncidentSensitiveFields(t, evidenceData)
	evidenceMeta := object(t, array(t, evidenceData["evidence"])[0])
	if evidenceMeta["view_available"] != true {
		t.Fatalf("configured evidence view was not advertised: %#v", evidenceMeta)
	}
	evidenceID := stringValue(t, evidenceMeta["id"])
	for _, test := range []struct {
		name      string
		path      string
		token     string
		actorType string
	}{
		{name: "rider", path: "/api/v1/delivery/incidents/" + damagedID + "/evidence/" + evidenceID + "/view", token: f.riderToken, actorType: "rider"},
		{name: "store", path: "/api/v1/store/delivery-incidents/" + damagedID + "/evidence/" + evidenceID + "/view", token: f.merchantToken, actorType: "merchant"},
		{name: "admin", path: "/api/v1/admin/delivery-incidents/" + damagedID + "/evidence/" + evidenceID + "/view", token: f.adminToken, actorType: "admin"},
	} {
		t.Run("evidence view "+test.name, func(t *testing.T) {
			response := performOK(t, f.router, http.MethodGet, test.path, test.token, "", nil)
			view := object(t, response["data"])
			viewURL := stringValue(t, view["url"])
			if strings.Contains(viewURL, "riders/") {
				t.Fatalf("temporary URL exposed object key: %s", viewURL)
			}
			parsed, err := url.Parse(viewURL)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := evidenceview.Open(f.cfg.DeliveryIncident.EvidenceViewSecret, parsed.Query().Get("token"), time.Now())
			if err != nil || claims.IncidentID != damagedID || claims.EvidenceID != evidenceID || claims.ActorType != test.actorType {
				t.Fatalf("temporary view claims=%+v err=%v", claims, err)
			}
		})
	}
	otherViewStatus, otherViewBody := perform(t, f.router, http.MethodGet, "/api/v1/store/delivery-incidents/"+damagedID+"/evidence/"+evidenceID+"/view", f.otherMerchantToken, "", nil)
	expectProblem(t, otherViewStatus, otherViewBody, http.StatusNotFound, "DELIVERY_INCIDENT_NOT_FOUND")
	var evidenceViewAudits int64
	if err := f.db.Table("audit_logs").Where("resource_type='delivery_incident_evidence' AND resource_id=? AND action='incident.evidence_view' AND result='success'", evidenceID).Count(&evidenceViewAudits).Error; err != nil || evidenceViewAudits != 3 {
		t.Fatalf("evidence view audit count=%d err=%v", evidenceViewAudits, err)
	}

	rejectFulfillment := f.createFulfillment(t, "delivering")
	rejectable := f.createIncident(t, rejectFulfillment, "customer_refused", "CUSTOMER_CHANGED_MIND")
	rejectID := stringValue(t, rejectable["id"])
	rejected := performOK(t, f.router, http.MethodPost, "/api/v1/admin/delivery-incidents/"+rejectID+"/reject", f.adminToken, "incident-reject-"+rejectID, map[string]any{"expected_version": 1, "reason_code": "NOT_AN_EXCEPTION", "reason": "facts do not support an incident"})
	if object(t, rejected["data"])["status"] != "rejected" {
		t.Fatalf("reject transition mismatch: %#v", rejected)
	}
	terminalStatus, terminalBody := perform(t, f.router, http.MethodPost, "/api/v1/admin/delivery-incidents/"+rejectID+"/resolve", f.adminToken, "incident-terminal-"+rejectID, map[string]any{"expected_version": 2, "resolution_code": "other", "resolution_note": "must not reopen"})
	expectProblem(t, terminalStatus, terminalBody, http.StatusConflict, "DELIVERY_INCIDENT_STATUS_CONFLICT")

	invalidFulfillment := f.createFulfillment(t, "accepted")
	invalidStatus, invalidBody := perform(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/delivery/orders/%d/incidents", invalidFulfillment.DeliveryID), f.riderToken, fmt.Sprintf("incident-invalid-stage-%d", invalidFulfillment.DeliveryID), map[string]any{"type": "customer_refused", "reason_code": "NO_LONGER_NEEDED", "description": "invalid at pickup"})
	expectProblem(t, invalidStatus, invalidBody, http.StatusConflict, "DELIVERY_INCIDENT_INVALID_STAGE")

	var deliveryStatus, orderStatus string
	if err := f.db.Table("delivery_orders").Select("status").Where("id=?", fulfillment.DeliveryID).Scan(&deliveryStatus).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("orders").Select("status").Where("id=?", fulfillment.OrderID).Scan(&orderStatus).Error; err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "accepted" || orderStatus != "paid" {
		t.Fatalf("incident operations changed fulfillment state: delivery=%s order=%s", deliveryStatus, orderStatus)
	}
	assertIncidentDurability(t, f.db, incidentID, 3, 3)
}

func (f *deliveryIncidentE2EFixture) testNaturalClosureEntrypoints(t *testing.T) {
	pickupFulfillment := f.createFulfillment(t, "accepted")
	pickupIncident := f.createIncident(t, pickupFulfillment, "out_of_stock", "")
	pickupID := stringValue(t, pickupIncident["id"])
	pickup := performOK(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/delivery/orders/%d/pickup", pickupFulfillment.DeliveryID), f.riderToken, fmt.Sprintf("delivery-pickup-%d", pickupFulfillment.DeliveryID), map[string]any{})
	if object(t, pickup["data"])["status"] != "delivering" {
		t.Fatalf("pickup did not advance delivery: %#v", pickup)
	}
	f.assertNaturalClose(t, pickupID, "pickup_resumed")

	completeFulfillment := f.createFulfillment(t, "delivering")
	completeIncident := f.createIncident(t, completeFulfillment, "customer_refused", "CUSTOMER_CHANGED_MIND")
	completeID := stringValue(t, completeIncident["id"])
	complete := performOK(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/delivery/orders/%d/complete", completeFulfillment.DeliveryID), f.riderToken, fmt.Sprintf("delivery-complete-%d", completeFulfillment.DeliveryID), map[string]any{})
	if object(t, complete["data"])["status"] != "completed" {
		t.Fatalf("complete did not advance delivery: %#v", complete)
	}
	f.assertNaturalClose(t, completeID, "delivery_completed")

	forceFulfillment := f.createFulfillment(t, "delivering")
	forceIncident := f.createIncident(t, forceFulfillment, "customer_refused", "NO_CONTACT")
	forceIncidentID := stringValue(t, forceIncident["id"])
	approval := performOK(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/admin/deliveries/%d/force-complete-requests", forceFulfillment.DeliveryID), f.adminToken, fmt.Sprintf("force-request-%d", forceFulfillment.DeliveryID), map[string]any{
		"checker_admin_id": strconv.FormatUint(f.checkerAdminID, 10), "reason_code": "CUSTOMER_CONFIRMED", "reason": "customer confirmed receipt", "expected_version": 1,
	})
	approvalID := stringValue(t, object(t, approval["data"])["id"])
	forced := performOK(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/admin/deliveries/%d/force-complete", forceFulfillment.DeliveryID), f.checkerToken, fmt.Sprintf("force-approve-%d", forceFulfillment.DeliveryID), map[string]any{"approval_id": approvalID, "expected_version": 1})
	if object(t, forced["data"])["status"] != "completed" {
		t.Fatalf("force complete did not advance delivery: %#v", forced)
	}
	f.assertNaturalClose(t, forceIncidentID, "force_completed")

	cancelFulfillment := f.createFulfillment(t, "accepted")
	cancelFirst := f.createIncident(t, cancelFulfillment, "out_of_stock", "")
	cancelSecond := f.createIncident(t, cancelFulfillment, "alcohol_damaged", "")
	cancelled := performOK(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/admin/orders/%d/cancel", cancelFulfillment.OrderID), f.adminToken, fmt.Sprintf("admin-cancel-%d", cancelFulfillment.OrderID), map[string]any{"reason_code": "CUSTOMER_REQUEST", "reason": "customer requested cancellation", "expected_version": 1})
	if object(t, cancelled["data"])["status"] != "refunding" {
		t.Fatalf("paid order cancel did not enter refund workflow: %#v", cancelled)
	}
	f.assertNaturalClose(t, stringValue(t, cancelFirst["id"]), "order_cancelled")
	f.assertNaturalClose(t, stringValue(t, cancelSecond["id"]), "order_cancelled")
	var active int64
	if err := f.db.Table("delivery_incidents").Where("delivery_order_id=? AND status IN ('evidence_required','open','acknowledged')", cancelFulfillment.DeliveryID).Count(&active).Error; err != nil || active != 0 {
		t.Fatalf("cancel left active incidents: count=%d err=%v", active, err)
	}
}

func (f *deliveryIncidentE2EFixture) testNaturalClosureFailureRollback(t *testing.T) {
	fulfillment := f.createFulfillment(t, "accepted")
	incident := f.createIncident(t, fulfillment, "out_of_stock", "")
	incidentID := stringValue(t, incident["id"])
	callbackName := "delivery_incident_e2e_fail_natural_close"
	if err := f.db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "delivery_incidents" {
			tx.AddError(errors.New("injected incident close failure"))
		}
	}); err != nil {
		t.Fatalf("register failure injection: %v", err)
	}
	status, body := perform(t, f.router, http.MethodPost, fmt.Sprintf("/api/v1/delivery/orders/%d/pickup", fulfillment.DeliveryID), f.riderToken, fmt.Sprintf("delivery-pickup-failure-%d", fulfillment.DeliveryID), map[string]any{})
	if err := f.db.Callback().Update().Remove(callbackName); err != nil {
		t.Fatalf("remove failure injection: %v", err)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("injected close failure should fail the main action: status=%d body=%#v", status, body)
	}
	var deliveryStatus, orderStatus, incidentStatus string
	f.db.Table("delivery_orders").Select("status").Where("id=?", fulfillment.DeliveryID).Scan(&deliveryStatus)
	f.db.Table("orders").Select("status").Where("id=?", fulfillment.OrderID).Scan(&orderStatus)
	f.db.Table("delivery_incidents").Select("status").Where("id=?", incidentID).Scan(&incidentStatus)
	if deliveryStatus != "accepted" || orderStatus != "paid" || incidentStatus != "open" {
		t.Fatalf("main transaction did not roll back: delivery=%s order=%s incident=%s", deliveryStatus, orderStatus, incidentStatus)
	}
	var resolvedHistory int64
	f.db.Table("delivery_incident_history").Where("incident_id=? AND action='resolved'", incidentID).Count(&resolvedHistory)
	if resolvedHistory != 0 {
		t.Fatalf("failed natural close committed %d resolved histories", resolvedHistory)
	}
}

func (f *deliveryIncidentE2EFixture) evidenceToken(t *testing.T, tokenID uint64) string {
	t.Helper()
	now := time.Now().UTC()
	claims := evidencetoken.Claims{
		Purpose: "delivery_incident_evidence", ObjectKey: fmt.Sprintf("riders/%d/%d.jpg", f.riderID, tokenID),
		MimeType: "image/jpeg", SizeBytes: 1024, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ScanStatus: "clean",
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "jxe-upload", Audience: jwt.ClaimStrings{"delivery-incident"}, Subject: fmt.Sprintf("rider:%d", f.riderID), ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(f.cfg.AfterSale.EvidenceTokenSecret))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *deliveryIncidentE2EFixture) assertNaturalClose(t *testing.T, incidentID, code string) {
	t.Helper()
	var row struct {
		Status         string
		ResolutionCode string
		Version        uint
	}
	if err := f.db.Table("delivery_incidents").Select("status,resolution_code,version").Where("id=?", incidentID).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "resolved" || row.ResolutionCode != code || row.Version != 2 {
		t.Fatalf("natural close mismatch for %s: %+v", incidentID, row)
	}
	assertIncidentDurability(t, f.db, incidentID, 2, 2)
}

func uintField(t *testing.T, object map[string]any, key string) uint {
	t.Helper()
	value, ok := object[key].(float64)
	if !ok || value < 0 {
		t.Fatalf("expected numeric %s, got %#v", key, object[key])
	}
	return uint(value)
}

func assertIncidentDurability(t *testing.T, db *gorm.DB, incidentID string, histories, events int64) {
	t.Helper()
	var historyCount, auditCount, eventCount int64
	if err := db.Table("delivery_incident_history").Where("incident_id=?", incidentID).Count(&historyCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("audit_logs").Where("resource_type='delivery_incident' AND resource_id=? AND action IN ? AND result='success'",
		incidentID, []string{"incident.report", "incident.evidence_add", "incident.acknowledge", "incident.resolve", "incident.reject", "incident.auto_resolve"}).Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("outbox_events").Where("aggregate_type='delivery_incident' AND aggregate_id=?", incidentID).Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if historyCount != histories || auditCount != histories || eventCount != events {
		t.Fatalf("incident durability mismatch history=%d audit=%d events=%d, want %d/%d/%d", historyCount, auditCount, eventCount, histories, histories, events)
	}
}

func assertNoIncidentSensitiveFields(t *testing.T, value any) {
	t.Helper()
	forbidden := map[string]bool{"object_key": true, "longitude": true, "latitude": true, "phone": true, "contact_phone": true, "evidence_tokens": true, "token_id": true, "sha256": true}
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, nested := range typed {
				if forbidden[key] {
					t.Fatalf("incident response exposed forbidden field %q", key)
				}
				walk(nested)
			}
		case []any:
			for _, nested := range typed {
				walk(nested)
			}
		}
	}
	walk(value)
}
