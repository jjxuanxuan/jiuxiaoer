package ops

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	testOperationAdminID = uint64(101)
	testManagerAdminID   = uint64(102)
)

func TestActiveAdminPermissionMatchesForceCompleteRoleSplit(t *testing.T) {
	db := newOpsAuthorizationDB(t)
	tests := []struct {
		name           string
		adminID        uint64
		permissionCode string
		want           bool
	}{
		{name: "operation can request", adminID: testOperationAdminID, permissionCode: "delivery:force_complete_request", want: true},
		{name: "operation cannot approve", adminID: testOperationAdminID, permissionCode: "delivery:force_complete_approve", want: false},
		{name: "admin manager cannot request", adminID: testManagerAdminID, permissionCode: "delivery:force_complete_request", want: false},
		{name: "admin manager can approve", adminID: testManagerAdminID, permissionCode: "delivery:force_complete_approve", want: true},
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
			return db.Table("permissions").Where("id = 2143").Update("status", "disabled").Error
		}},
		{name: "mapping revoked", mutate: func(db *gorm.DB) error {
			return db.Table("role_permissions").Where("role_id = 1003 AND permission_id = 2143").Update("deleted_at", time.Now()).Error
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newOpsAuthorizationDB(t)
			if err := tt.mutate(db); err != nil {
				t.Fatal(err)
			}
			allowed, err := activeAdminHasPermission(context.Background(), db, testOperationAdminID, "delivery:force_complete_request")
			if err != nil {
				t.Fatal(err)
			}
			if allowed {
				t.Fatal("inactive authorization-chain record retained force-complete permission")
			}
		})
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
		Permissions: []string{"delivery:force_complete_request"},
	}
	if err := db.Table("role_permissions").Where("role_id = 1003 AND permission_id = 2143").Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	_, err := service.RequestForceComplete(ctx, operationClaims, "POST", "/force-complete-requests", "revoked-maker-key", "1", ForceCompleteRequestReq{
		CheckerAdminID: fmt.Sprint(testManagerAdminID), ReasonCode: "CUSTOMER_CONFIRMED", Reason: "customer confirmed", ExpectedVersion: 1,
	})
	assertProblemCode(t, err, "PERM_FORBIDDEN")

	if err := db.Table("role_permissions").Where("role_id = 1003 AND permission_id = 2143").Update("deleted_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("role_permissions").Where("role_id = 1002 AND permission_id = 2144").Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.RequestForceComplete(ctx, operationClaims, "POST", "/force-complete-requests", "revoked-checker-key", "1", ForceCompleteRequestReq{
		CheckerAdminID: fmt.Sprint(testManagerAdminID), ReasonCode: "CUSTOMER_CONFIRMED", Reason: "customer confirmed", ExpectedVersion: 1,
	})
	assertProblemCode(t, err, "CHECKER_PERMISSION_REQUIRED")

	managerClaims := &auth.Claims{
		AccountType: "admin",
		AdminUserID: fmt.Sprint(testManagerAdminID),
		Permissions: []string{"delivery:force_complete_approve"},
	}
	_, err = service.ForceComplete(ctx, managerClaims, "POST", "/force-complete", "revoked-approver-key", "1", ForceCompleteReq{ApprovalID: "1", ExpectedVersion: 1})
	assertProblemCode(t, err, "PERM_FORBIDDEN")
}

func assertProblemCode(t *testing.T, err error, want string) {
	t.Helper()
	details := problem.FromError(err)
	if details == nil || details.ErrorCode != want {
		t.Fatalf("error = %v, code = %v, want %s", err, details, want)
	}
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
		`INSERT INTO permissions (id, code, status) VALUES (2143, 'delivery:force_complete_request', 'active'), (2144, 'delivery:force_complete_approve', 'active')`,
		`INSERT INTO role_permissions (id, role_id, permission_id) VALUES (1005143, 1003, 2143), (1004144, 1002, 2144)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("setup authorization database: %v", err)
		}
	}
	return db
}
