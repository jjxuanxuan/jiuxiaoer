package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	testOperationAdminID = uint64(101)
	testManagerAdminID   = uint64(102)
	testForceCompleteID  = uint64(2068)
)

func TestRegisterRoutesExposesOnlyDirectForceComplete(t *testing.T) {
	router := gin.New()
	RegisterRoutes(router.Group(""), NewHandler(nil))

	var direct bool
	for _, route := range router.Routes() {
		if route.Method != "POST" {
			continue
		}
		switch route.Path {
		case "/deliveries/:id/force-complete":
			direct = true
		case "/deliveries/:id/force-complete-requests":
			t.Fatal("legacy force-complete request route is still registered")
		}
	}
	if !direct {
		t.Fatal("direct force-complete route is not registered")
	}
}

func TestActiveAdminPermissionMatchesForceCompletePermission(t *testing.T) {
	db := newOpsAuthorizationDB(t)
	tests := []struct {
		name           string
		adminID        uint64
		permissionCode string
		want           bool
	}{
		{name: "authorized administrator can force complete", adminID: testOperationAdminID, permissionCode: "delivery:force_complete", want: true},
		{name: "administrator without permission cannot force complete", adminID: testManagerAdminID, permissionCode: "delivery:force_complete", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := activeAdminHasPermission(context.Background(), db, tt.adminID, tt.permissionCode)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("activeAdminHasPermission() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestActiveAdminPermissionRequiresEntireAuthorizationChain(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gorm.DB) error
	}{
		{name: "account disabled", mutate: func(db *gorm.DB) error {
			return db.Table("accounts").Where("id = 11").Update("status", "disabled").Error
		}},
		{name: "admin disabled", mutate: func(db *gorm.DB) error {
			return db.Table("admin_users").Where("id = ?", testOperationAdminID).Update("status", "disabled").Error
		}},
		{name: "role disabled", mutate: func(db *gorm.DB) error {
			return db.Table("roles").Where("id = 1003").Update("status", "disabled").Error
		}},
		{name: "permission disabled", mutate: func(db *gorm.DB) error {
			return db.Table("permissions").Where("id = ?", testForceCompleteID).Update("status", "disabled").Error
		}},
		{name: "mapping revoked", mutate: func(db *gorm.DB) error {
			return db.Table("role_permissions").Where("role_id = 1003 AND permission_id = ?", testForceCompleteID).Update("deleted_at", time.Now()).Error
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newOpsAuthorizationDB(t)
			if err := tt.mutate(db); err != nil {
				t.Fatal(err)
			}
			allowed, err := activeAdminHasPermission(context.Background(), db, testOperationAdminID, "delivery:force_complete")
			if err != nil {
				t.Fatal(err)
			}
			if allowed {
				t.Fatal("inactive authorization-chain record retained force-complete permission")
			}
		})
	}
}

func TestForceCompleteExecutesDirectlyAndPreservesApprovalHistory(t *testing.T) {
	db := newOpsForceCompleteDB(t)
	cfg := config.Config{}
	cfg.CP1.ForceActionEnabled = true
	cfg.App.InstanceID = "ops-force-complete-test"
	service := NewService(cfg, db, snowflake.New(41), nil)
	ctx := requestctx.WithRequestID(
		context.Background(),
		"ops-force-complete-request",
	)
	claims := &auth.Claims{
		AccountType: "admin",
		AdminUserID: fmt.Sprint(testOperationAdminID),
		Permissions: []string{"delivery:force_complete"},
	}
	req := ForceCompleteReq{
		ReasonCode:      "CUSTOMER_CONFIRMED",
		Reason:          "customer confirmed receipt",
		ExpectedVersion: 3,
	}

	got, err := service.ForceComplete(ctx, claims, "POST", "/deliveries/:id/force-complete", "direct-force-complete", "301", req)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "301" || got.OrderID != "401" || got.Status != "completed" || got.CompletedAt == "" {
		t.Fatalf("ForceComplete() = %+v", got)
	}

	var delivery Delivery
	if err := db.First(&delivery, 301).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "completed" || delivery.CompletedAt == nil {
		t.Fatalf("delivery = %+v", delivery)
	}
	var orderRow Order
	if err := db.First(&orderRow, 401).Error; err != nil {
		t.Fatal(err)
	}
	if orderRow.Status != "completed" || orderRow.DeliveryStatus != "completed" || orderRow.CompletedAt == nil || orderRow.Version != 8 {
		t.Fatalf("order = %+v", orderRow)
	}

	var verification deliveryverification.Verification
	if err := db.First(&verification, 501).Error; err != nil {
		t.Fatal(err)
	}
	if verification.Status != "overridden" || verification.VerifiedByID == nil || *verification.VerifiedByID != testOperationAdminID {
		t.Fatalf("verification actor/status = %+v", verification)
	}
	if verification.OverrideReasonCode == nil || *verification.OverrideReasonCode != req.ReasonCode ||
		verification.OverrideReason == nil || *verification.OverrideReason != req.Reason {
		t.Fatalf("verification reason = %+v", verification)
	}
	if verification.Version != 2 {
		t.Fatalf("overridden verification version=%d, want 2", verification.Version)
	}
	var terminalVerifications []deliveryverification.Verification
	if err := db.
		Where("id IN ?", []uint64{502, 503, 504}).
		Order("id ASC").
		Find(&terminalVerifications).Error; err != nil {
		t.Fatal(err)
	}
	if len(terminalVerifications) != 3 ||
		terminalVerifications[0].Status != "expired" ||
		terminalVerifications[1].Status != "invalidated" ||
		terminalVerifications[2].Status != "verified" {
		t.Fatalf(
			"force complete rewrote terminal verifications: %+v",
			terminalVerifications,
		)
	}

	var approvalStatus string
	if err := db.Table("admin_override_approvals").Select("status").Where("id = 601").Scan(&approvalStatus).Error; err != nil {
		t.Fatal(err)
	}
	if approvalStatus != "pending" {
		t.Fatalf("historical approval status = %q, want pending", approvalStatus)
	}

	var auditRow struct {
		ActorID      uint64
		Action       string
		BeforeData   []byte
		AfterData    []byte
		BeforeStatus *string
		AfterStatus  *string
		Version      *uint64
		RequestID    *string
	}
	if err := db.Table("audit_logs").Where("resource_id = 301").Take(&auditRow).Error; err != nil {
		t.Fatal(err)
	}
	if auditRow.ActorID != testOperationAdminID || auditRow.Action != "delivery.force_complete" {
		t.Fatalf("audit actor/action = %+v", auditRow)
	}
	var before, after map[string]any
	if err := json.Unmarshal(auditRow.BeforeData, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(auditRow.AfterData, &after); err != nil {
		t.Fatal(err)
	}
	if auditRow.BeforeStatus == nil || *auditRow.BeforeStatus != "delivering" ||
		auditRow.AfterStatus == nil || *auditRow.AfterStatus != "completed" ||
		auditRow.Version == nil || *auditRow.Version != 3 ||
		auditRow.RequestID == nil || *auditRow.RequestID != requestctx.RequestID(ctx) ||
		before["status"] != "delivering" || before["version"] != float64(3) ||
		before["order_status"] != "paid" || before["order_version"] != float64(7) ||
		after["actor_admin_id"] != fmt.Sprint(testOperationAdminID) ||
		after["permission"] != "delivery:force_complete" ||
		after["reason_code"] != req.ReasonCode ||
		after["status"] != "completed" || after["version"] != float64(3) ||
		after["order_status"] != "completed" || after["order_version"] != float64(8) ||
		after["request_id"] != requestctx.RequestID(ctx) ||
		after["correlation_id"] != requestctx.RequestID(ctx) ||
		after["idempotency_key_hash"] != idempotency.KeyHash("direct-force-complete") ||
		after["service_instance"] != cfg.App.InstanceID {
		t.Fatalf("incomplete force-complete audit=%+v before=%#v after=%#v", auditRow, before, after)
	}

	replayed, err := service.ForceComplete(ctx, claims, "POST", "/deliveries/:id/force-complete", "direct-force-complete", "301", req)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != got.Status || replayed.CompletedAt != got.CompletedAt {
		t.Fatalf("idempotent replay = %+v, want %+v", replayed, got)
	}
	var auditCount, eventCount int64
	if err := db.Table("audit_logs").
		Where("resource_id=301 AND action='delivery.force_complete'").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Outbox{}).
		Where(
			"(aggregate_type='delivery_order' AND aggregate_id=301) OR (aggregate_type='order' AND aggregate_id=401)",
		).
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || eventCount != 2 {
		t.Fatalf(
			"idempotent replay duplicated effects audit=%d events=%d",
			auditCount,
			eventCount,
		)
	}
}

func TestForceCompleteConflictsAndReturnGuardHaveZeroSideEffects(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		req      ForceCompleteReq
		returns  ReturnGuard
		wantCode string
	}{
		{
			name: "version conflict",
			key:  "force-version-conflict",
			req: ForceCompleteReq{
				ReasonCode:      "CUSTOMER_CONFIRMED",
				Reason:          "customer confirmed receipt",
				ExpectedVersion: 99,
			},
			wantCode: "VERSION_CONFLICT",
		},
		{
			name: "active return",
			key:  "force-active-return",
			req: ForceCompleteReq{
				ReasonCode:      "CUSTOMER_CONFIRMED",
				Reason:          "customer confirmed receipt",
				ExpectedVersion: 3,
			},
			returns:  activeOpsReturnGuard{},
			wantCode: "INVALID_RETURN_STATE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newOpsForceCompleteDB(t)
			cfg := config.Config{}
			cfg.CP1.ForceActionEnabled = true
			service := NewService(cfg, db, snowflake.New(42), nil).
				WithReturnGuard(tt.returns)
			claims := &auth.Claims{
				AccountType: "admin",
				AdminUserID: fmt.Sprint(testOperationAdminID),
				Permissions: []string{"delivery:force_complete"},
			}
			_, err := service.ForceComplete(
				context.Background(),
				claims,
				"POST",
				"/deliveries/:id/force-complete",
				tt.key,
				"301",
				tt.req,
			)
			assertProblemCode(t, err, tt.wantCode)

			var delivery Delivery
			if err := db.First(&delivery, 301).Error; err != nil {
				t.Fatal(err)
			}
			var orderRow Order
			if err := db.First(&orderRow, 401).Error; err != nil {
				t.Fatal(err)
			}
			var verification deliveryverification.Verification
			if err := db.First(&verification, 501).Error; err != nil {
				t.Fatal(err)
			}
			var audits, events, idempotencyRows int64
			_ = db.Table("audit_logs").Count(&audits).Error
			_ = db.Model(&Outbox{}).Count(&events).Error
			_ = db.Table("idempotency_keys").Count(&idempotencyRows).Error
			if delivery.Status != "delivering" ||
				orderRow.Status != "paid" ||
				orderRow.DeliveryStatus != "delivering" ||
				verification.Status != "active" ||
				audits != 0 ||
				events != 0 ||
				idempotencyRows != 0 {
				t.Fatalf(
					"conflict side effects delivery=%s order=%s/%s verification=%s audits=%d events=%d idempotency=%d",
					delivery.Status,
					orderRow.Status,
					orderRow.DeliveryStatus,
					verification.Status,
					audits,
					events,
					idempotencyRows,
				)
			}
		})
	}
}

func TestForceCompleteInfrastructureLookupErrorIsNotNotFound(t *testing.T) {
	db := newOpsForceCompleteDB(t)
	callbackName := "ops:inject-delivery-query-failure"
	if err := db.Callback().Query().Before("gorm:query").Register(
		callbackName,
		func(tx *gorm.DB) {
			if tx.Statement.Table == "delivery_orders" {
				tx.AddError(errors.New("injected delivery query failure"))
			}
		},
	); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove(callbackName)

	cfg := config.Config{}
	cfg.CP1.ForceActionEnabled = true
	service := NewService(cfg, db, snowflake.New(43), nil)
	claims := &auth.Claims{
		AccountType: "admin",
		AdminUserID: fmt.Sprint(testOperationAdminID),
		Permissions: []string{"delivery:force_complete"},
	}
	_, err := service.ForceComplete(
		context.Background(),
		claims,
		"POST",
		"/deliveries/:id/force-complete",
		"force-query-failure",
		"301",
		ForceCompleteReq{
			ReasonCode:      "CUSTOMER_CONFIRMED",
			Reason:          "customer confirmed receipt",
			ExpectedVersion: 3,
		},
	)
	details := problem.FromError(err)
	if details.Status != 500 || details.ErrorCode == "DELIVERY_NOT_FOUND" {
		t.Fatalf("infrastructure lookup error=%+v", details)
	}
}

func TestForceCompleteRejectsPermissionRevokedAfterTokenIssuance(t *testing.T) {
	db := newOpsAuthorizationDB(t)
	cfg := config.Config{}
	cfg.CP1.ForceActionEnabled = true
	service := NewService(cfg, db, snowflake.New(41), nil)
	ctx := context.Background()

	operationClaims := &auth.Claims{
		AccountType: "admin",
		AdminUserID: fmt.Sprint(testOperationAdminID),
		Permissions: []string{"delivery:force_complete"},
	}
	if err := db.Table("role_permissions").Where("role_id = 1003 AND permission_id = ?", testForceCompleteID).Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	_, err := service.ForceComplete(ctx, operationClaims, "POST", "/force-complete", "revoked-admin-key", "1", ForceCompleteReq{
		ReasonCode: "CUSTOMER_CONFIRMED", Reason: "customer confirmed", ExpectedVersion: 1,
	})
	assertProblemCode(t, err, "PERM_FORBIDDEN")
}

func assertProblemCode(t *testing.T, err error, want string) {
	t.Helper()
	details := problem.FromError(err)
	if details == nil || details.ErrorCode != want {
		t.Fatalf("error = %v, code = %v, want %s", err, details, want)
	}
}

type activeOpsReturnGuard struct{}

func (activeOpsReturnGuard) HasActiveLocked(
	context.Context,
	*gorm.DB,
	uint64,
) (bool, error) {
	return true, nil
}

func newOpsAuthorizationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE accounts (id INTEGER PRIMARY KEY, account_type TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE admin_users (id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, role_id INTEGER NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE permissions (id INTEGER PRIMARY KEY, code TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE role_permissions (id INTEGER PRIMARY KEY, role_id INTEGER NOT NULL, permission_id INTEGER NOT NULL, deleted_at DATETIME)`,
		`INSERT INTO accounts (id, account_type, status) VALUES (11, 'admin', 'active'), (12, 'admin', 'active')`,
		`INSERT INTO roles (id, code, status) VALUES (1003, 'operation', 'active'), (1002, 'admin_manager', 'active')`,
		`INSERT INTO admin_users (id, account_id, role_id, status) VALUES (101, 11, 1003, 'active'), (102, 12, 1002, 'active')`,
		fmt.Sprintf(`INSERT INTO permissions (id, code, status) VALUES (%d, 'delivery:force_complete', 'active')`, testForceCompleteID),
		fmt.Sprintf(`INSERT INTO role_permissions (id, role_id, permission_id) VALUES (1005068, 1003, %d)`, testForceCompleteID),
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("setup authorization database: %v", err)
		}
	}
	return db
}

func newOpsForceCompleteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newOpsAuthorizationDB(t)
	if err := db.AutoMigrate(
		&Delivery{},
		&Order{},
		&Outbox{},
		&deliveryverification.Verification{},
		&idempotency.Record{},
	); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE UNIQUE INDEX uq_ops_idempotency ON idempotency_keys (actor_type, actor_id, path, key_hash)`,
		`CREATE TABLE audit_logs (
			id INTEGER PRIMARY KEY,
			event_id TEXT,
			actor_type TEXT NOT NULL,
			actor_id INTEGER NOT NULL,
			action TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			resource_id INTEGER NOT NULL,
			before_data JSON,
			after_data JSON NOT NULL,
			result TEXT NOT NULL,
			before_status TEXT,
			after_status TEXT,
			version INTEGER,
			request_id TEXT,
			ip_hash TEXT,
			user_agent TEXT,
			account_id INTEGER
		)`,
		`CREATE TABLE admin_override_approvals (
			id INTEGER PRIMARY KEY,
			action TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			resource_id INTEGER NOT NULL,
			status TEXT NOT NULL
		)`,
		`INSERT INTO delivery_orders (id, order_id, shop_id, status, assignment_version, updated_at)
			VALUES (301, 401, 201, 'delivering', 3, CURRENT_TIMESTAMP)`,
		`INSERT INTO orders (id, status, delivery_status, version)
			VALUES (401, 'paid', 'delivering', 7)`,
		`INSERT INTO delivery_verifications (
			id, delivery_order_id, stage, mode_snapshot, status, failed_attempts,
			max_attempts, expires_at, version, created_at, updated_at
		) VALUES (
			501, 301, 'delivery', 'enforce', 'active', 0,
			5, DATETIME('now', '+30 minutes'), 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)`,
		`INSERT INTO delivery_verifications (
			id, delivery_order_id, stage, mode_snapshot, status, failed_attempts,
			max_attempts, expires_at, version, created_at, updated_at
		) VALUES
			(502, 301, 'delivery', 'enforce', 'expired', 0, 5,
			 DATETIME('now', '-30 minutes'), 4, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			(503, 301, 'delivery', 'enforce', 'invalidated', 0, 5,
			 DATETIME('now', '+30 minutes'), 5, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			(504, 301, 'delivery', 'enforce', 'verified', 0, 5,
			 DATETIME('now', '+30 minutes'), 6, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO admin_override_approvals (id, action, resource_type, resource_id, status)
			VALUES (601, 'delivery.force_complete', 'delivery_order', 301, 'pending')`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("setup force-complete database: %v", err)
		}
	}
	return db
}
