package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestMerchantIdentityLoadsRoleAndPermissionsFromDatabase(t *testing.T) {
	db := merchantAuthSQLite(t)
	statements := []string{
		`CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL, scope TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE permissions (id INTEGER PRIMARY KEY, code TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE role_permissions (id INTEGER PRIMARY KEY, role_id INTEGER NOT NULL, permission_id INTEGER NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE merchant_users (id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, merchant_id INTEGER NOT NULL, role_id INTEGER NOT NULL, name TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE merchant_user_shops (id INTEGER PRIMARY KEY, merchant_user_id INTEGER NOT NULL, shop_id INTEGER NOT NULL, deleted_at DATETIME)`,
		`INSERT INTO roles(id,code,scope,status) VALUES (1007,'merchant_order_operator','merchant','active'),(1008,'merchant_inventory_clerk','merchant','active')`,
		`INSERT INTO permissions(id,code,status) VALUES (2014,'store_order:accept','active'),(2006,'inventory:adjust','active')`,
		`INSERT INTO role_permissions(id,role_id,permission_id) VALUES (1,1007,2014),(2,1008,2006)`,
		`INSERT INTO merchant_users(id,account_id,merchant_id,role_id,name,status) VALUES (11,1,21,1007,'operator','active')`,
		`INSERT INTO merchant_user_shops(id,merchant_user_id,shop_id) VALUES (31,11,41)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare merchant RBAC database: %v", err)
		}
	}

	service := NewService(config.Load(), db, nil, snowflake.New(721))
	account := Account{ID: 1, AccountType: "merchant", Status: "active", CredentialVersion: 3}
	identity, err := service.identityForAccount(context.Background(), account)
	if err != nil {
		t.Fatalf("load order operator identity: %v", err)
	}
	if identity.RoleCode != "merchant_order_operator" || identity.CredentialVersion != 3 {
		t.Fatalf("unexpected merchant role snapshot: %+v", identity)
	}
	if !hasPermission(identity.Permissions, "store_order:accept") || hasPermission(identity.Permissions, "inventory:adjust") {
		t.Fatalf("order operator permission boundary is wrong: %v", identity.Permissions)
	}
	if got := identity.Profile["role_code"]; got != "merchant_order_operator" {
		t.Fatalf("profile role_code=%v", got)
	}

	if err := db.Exec("UPDATE merchant_users SET role_id=1008 WHERE id=11").Error; err != nil {
		t.Fatal(err)
	}
	identity, err = service.identityForAccount(context.Background(), account)
	if err != nil {
		t.Fatalf("load inventory clerk identity: %v", err)
	}
	if identity.RoleCode != "merchant_inventory_clerk" || !hasPermission(identity.Permissions, "inventory:adjust") || hasPermission(identity.Permissions, "store_order:accept") {
		t.Fatalf("inventory clerk permission boundary is wrong: role=%s permissions=%v", identity.RoleCode, identity.Permissions)
	}
}

func TestMerchantIdentityRejectsNonMerchantOrInactiveRole(t *testing.T) {
	db := merchantAuthSQLite(t)
	for _, statement := range []string{
		`CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL, scope TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE permissions (id INTEGER PRIMARY KEY, code TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE role_permissions (id INTEGER PRIMARY KEY, role_id INTEGER NOT NULL, permission_id INTEGER NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE merchant_users (id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, merchant_id INTEGER NOT NULL, role_id INTEGER NOT NULL, name TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE merchant_user_shops (id INTEGER PRIMARY KEY, merchant_user_id INTEGER NOT NULL, shop_id INTEGER NOT NULL, deleted_at DATETIME)`,
		`INSERT INTO roles(id,code,scope,status) VALUES (1001,'super_admin','all','active'),(1007,'merchant_order_operator','merchant','disabled')`,
		`INSERT INTO merchant_users(id,account_id,merchant_id,role_id,name,status) VALUES (11,1,21,1001,'bad scope','active'),(12,2,21,1007,'inactive','active')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(config.Load(), db, nil, snowflake.New(722))
	for _, accountID := range []uint64{1, 2} {
		_, err := service.identityForAccount(context.Background(), Account{ID: accountID, AccountType: "merchant", Status: "active", CredentialVersion: 1})
		if problem.FromError(err).ErrorCode != "MERCHANT_ROLE_INVALID" {
			t.Fatalf("account %d with invalid role was accepted: %v", accountID, err)
		}
	}
}

func TestCredentialVersionRevokesOldPermissionSnapshot(t *testing.T) {
	now := time.Now()
	account := Account{ID: 1, CredentialVersion: 2, TokenInvalidBefore: &now}
	old := &Claims{CredentialVersion: 1}
	if err := validateAccountTokenSnapshot(account, old); problem.FromError(err).ErrorCode != "AUTH_SESSION_REVOKED" {
		t.Fatalf("old credential version was accepted: %v", err)
	}
	current := &Claims{CredentialVersion: 2}
	if err := validateAccountTokenSnapshot(account, current); err != nil {
		t.Fatalf("current credential version was rejected: %v", err)
	}
	legacy := &Claims{}
	if err := validateAccountTokenSnapshot(account, legacy); problem.FromError(err).ErrorCode != "AUTH_SESSION_REVOKED" {
		t.Fatalf("legacy permission snapshot was accepted: %v", err)
	}
}

func merchantAuthSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func hasPermission(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
