package ops

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestSlotAdminRealLoginJWTEnforcesShopScope(t *testing.T) {
	slotService, db, _ := newSlotAdminTestService(t)
	seedSlotAdminShop(t, db, 201, 101, "310100")
	seedSlotAdminShop(t, db, 202, 102, "310100")
	prepareSlotAdminAuthSchema(t, db)

	cfg := config.Load()
	cfg.JWT.AccessSecret = "slot-admin-scope-access-secret"
	cfg.JWT.RefreshSecret = "slot-admin-scope-refresh-secret"
	cfg.JWT.AccessTTL = time.Hour
	cfg.JWT.RefreshTTL = 24 * time.Hour
	mini := miniredis.RunT(t)
	redisClient := goredis.NewClient(&goredis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	loginService := auth.NewService(
		cfg,
		db,
		redisClient,
		snowflake.New(477),
	)

	seedSlotAdminLoginPrincipal(
		t,
		db,
		1001,
		2001,
		3001,
		"scoped_operation",
		"operation",
		"scoped",
		201,
	)
	seedSlotAdminLoginPrincipal(
		t,
		db,
		1002,
		2002,
		3002,
		"empty_operation",
		"operation",
		"scoped",
	)
	seedSlotAdminLoginPrincipal(
		t,
		db,
		1003,
		2003,
		3003,
		"global_manager",
		"admin_manager",
		"all",
	)

	scopedClaims := loginSlotAdminClaims(
		t,
		loginService,
		"scoped_operation",
	)
	if scopedClaims.RoleCode != "operation" ||
		!reflect.DeepEqual(scopedClaims.AuthorizedShopIDs, []string{"201"}) {
		t.Fatalf("unexpected scoped JWT claims: %+v", scopedClaims)
	}
	authorized, err := slotService.Create(
		context.Background(),
		scopedClaims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"real-login-authorized-shop",
		validSlotAdminCreateRequest(),
	)
	if err != nil || authorized.ShopID != "201" {
		t.Fatalf("authorized shop create=%+v err=%v", authorized, err)
	}

	crossShop := validSlotAdminCreateRequest()
	crossShop.ShopID = "202"
	_, err = slotService.Create(
		context.Background(),
		scopedClaims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"real-login-cross-shop",
		crossShop,
	)
	assertSlotAdminForbidden(t, err)

	emptyClaims := loginSlotAdminClaims(
		t,
		loginService,
		"empty_operation",
	)
	if emptyClaims.RoleCode != "operation" ||
		len(emptyClaims.AuthorizedShopIDs) != 0 {
		t.Fatalf("unexpected empty-scope JWT claims: %+v", emptyClaims)
	}
	_, err = slotService.Create(
		context.Background(),
		emptyClaims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"real-login-empty-scope",
		crossShop,
	)
	assertSlotAdminForbidden(t, err)

	globalClaims := loginSlotAdminClaims(
		t,
		loginService,
		"global_manager",
	)
	if globalClaims.RoleCode != "admin_manager" ||
		len(globalClaims.AuthorizedShopIDs) != 0 {
		t.Fatalf("unexpected global JWT claims: %+v", globalClaims)
	}
	global, err := slotService.Create(
		context.Background(),
		globalClaims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"real-login-global-shop",
		crossShop,
	)
	if err != nil || global.ShopID != "202" {
		t.Fatalf("global shop create=%+v err=%v", global, err)
	}
}

func prepareSlotAdminAuthSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(
		&auth.Account{},
		&auth.AdminUser{},
		&auth.AdminUserShop{},
		&auth.AuditLog{},
	); err != nil {
		t.Fatalf("migrate admin auth fixtures: %v", err)
	}
	if err := db.Exec(
		"ALTER TABLE admin_users ADD COLUMN deleted_at DATETIME",
	).Error; err != nil {
		t.Fatalf("add admin_users.deleted_at: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE roles (
			id INTEGER PRIMARY KEY,
			code TEXT NOT NULL,
			scope TEXT NOT NULL,
			status TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE permissions (
			id INTEGER PRIMARY KEY,
			code TEXT NOT NULL,
			status TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE role_permissions (
			id INTEGER PRIMARY KEY,
			role_id INTEGER NOT NULL,
			permission_id INTEGER NOT NULL,
			deleted_at DATETIME
		)`,
		`INSERT INTO permissions (
			id, code, status
		) VALUES (
			4001, 'wine_ticket_slot:create', 'active'
		)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare admin auth schema: %v", err)
		}
	}
}

func seedSlotAdminLoginPrincipal(
	t *testing.T,
	db *gorm.DB,
	accountID uint64,
	adminID uint64,
	roleID uint64,
	username string,
	roleCode string,
	roleScope string,
	shopIDs ...uint64,
) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword(
		[]byte("slot-admin-password"),
		bcrypt.MinCost,
	)
	if err != nil {
		t.Fatal(err)
	}
	usernameCopy := username
	hashString := string(hash)
	if err := db.Create(&auth.Account{
		ID: accountID, AccountType: "admin",
		Username: &usernameCopy, PasswordHash: &hashString,
		Status: "active", CredentialVersion: 1,
	}).Error; err != nil {
		t.Fatalf("create account %s: %v", username, err)
	}
	if err := db.Exec(
		`INSERT INTO roles (
			id, code, scope, status
		) VALUES (?, ?, ?, 'active')`,
		roleID,
		roleCode,
		roleScope,
	).Error; err != nil {
		t.Fatalf("create role %s: %v", roleCode, err)
	}
	if err := db.Create(&auth.AdminUser{
		ID: adminID, AccountID: accountID, RoleID: roleID,
		AdminSubRole: roleCode, Name: username, Status: "active",
	}).Error; err != nil {
		t.Fatalf("create admin %s: %v", username, err)
	}
	if err := db.Exec(
		`INSERT INTO role_permissions (
			id, role_id, permission_id
		) VALUES (?, ?, 4001)`,
		roleID+10000,
		roleID,
	).Error; err != nil {
		t.Fatalf("grant role %s: %v", roleCode, err)
	}
	for index, shopID := range shopIDs {
		if err := db.Create(&auth.AdminUserShop{
			ID:          adminID*100 + uint64(index+1),
			AdminUserID: adminID,
			ShopID:      shopID,
		}).Error; err != nil {
			t.Fatalf("grant admin %s shop %d: %v", username, shopID, err)
		}
	}
}

func loginSlotAdminClaims(
	t *testing.T,
	service *auth.Service,
	username string,
) *auth.Claims {
	t.Helper()
	response, err := service.PasswordLogin(
		context.Background(),
		"admin",
		auth.PasswordLoginReq{
			Username: username,
			Password: "slot-admin-password",
		},
	)
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	claims, err := service.VerifyAccess(
		context.Background(),
		response.AccessToken,
	)
	if err != nil {
		t.Fatalf("verify %s access token: %v", username, err)
	}
	return claims
}

func assertSlotAdminForbidden(t *testing.T, err error) {
	t.Helper()
	details := problem.FromError(err)
	if details.Status != http.StatusForbidden ||
		details.ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("problem=%+v, want 403 PERM_FORBIDDEN", details)
	}
}
