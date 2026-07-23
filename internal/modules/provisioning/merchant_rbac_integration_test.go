package provisioning

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestUpdateMerchantRoleRevokesTokenSnapshotMySQL(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run merchant provisioning RBAC integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysqlinfra.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()

	ids := snowflake.New(724)
	accountID, merchantUserID, merchantID := ids.Next(), ids.Next(), ids.Next()
	shopID, foreignShopID, foreignMerchantID := ids.Next(), ids.Next(), ids.Next()
	if err := tx.Exec(`INSERT INTO accounts (id,account_type,username,status,credential_version) VALUES (?,'merchant',?,'active',1)`, accountID, "provision_rbac_"+strconv.FormatUint(accountID, 10)).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`INSERT INTO merchant_users (id,account_id,merchant_id,role_id,name,status) VALUES (?,?,?,?,?,'active')`, merchantUserID, accountID, merchantID, uint64(1006), "provision RBAC").Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`INSERT INTO shops (id,merchant_id,name,city,district,address,status,business_status) VALUES
		(?,?,?,'深圳市','南山区','RBAC own','active','open'),
		(?,?,?,'深圳市','南山区','RBAC foreign','active','open')`,
		shopID, merchantID, "RBAC own", foreignShopID, foreignMerchantID, "RBAC foreign").Error; err != nil {
		t.Fatal(err)
	}

	cp1 := cfg.CP1
	cp1.ProvisioningEnabled = true
	service := NewService(cp1, tx, ids)
	claims := &auth.Claims{AccountType: "admin", AdminUserID: "9001", Permissions: []string{"merchant_user:update_role"}}
	path := "/api/v1/admin/merchant-users/:id/role"
	key := "merchant-role-change-1"
	result, err := service.UpdateMerchantUserRole(ctx, claims, "PATCH", path, key, strconv.FormatUint(merchantUserID, 10), MerchantUserRoleReq{RoleCode: MerchantRoleInventoryClerk})
	if err != nil {
		t.Fatalf("update merchant role: %v", err)
	}
	if result.TargetID != strconv.FormatUint(merchantUserID, 10) || result.Status != "succeeded" {
		t.Fatalf("unexpected operation: %+v", result)
	}
	var state struct {
		RoleID             uint64
		CredentialVersion  uint
		TokenInvalidBefore *time.Time
	}
	if err := tx.Table("merchant_users mu").
		Select("mu.role_id,a.credential_version,a.token_invalid_before").
		Joins("JOIN accounts a ON a.id=mu.account_id").
		Where("mu.id=?", merchantUserID).
		Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.RoleID != 1008 || state.CredentialVersion != 2 || state.TokenInvalidBefore == nil {
		t.Fatalf("role and token version were not changed atomically: %+v", state)
	}

	replay, err := service.UpdateMerchantUserRole(ctx, claims, "PATCH", path, key, strconv.FormatUint(merchantUserID, 10), MerchantUserRoleReq{RoleCode: MerchantRoleInventoryClerk})
	if err != nil || replay.ID != result.ID {
		t.Fatalf("idempotent replay=%+v err=%v", replay, err)
	}
	var version uint
	if err := tx.Table("accounts").Select("credential_version").Where("id=?", accountID).Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("idempotent replay advanced credential_version to %d", version)
	}

	withoutPermission := *claims
	withoutPermission.Permissions = nil
	if _, err := service.UpdateMerchantUserRole(ctx, &withoutPermission, "PATCH", path, "merchant-role-change-2", strconv.FormatUint(merchantUserID, 10), MerchantUserRoleReq{RoleCode: MerchantRoleOwner}); problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("role update without exact permission was accepted: %v", err)
	}
	if _, err := service.UpdateMerchantUserRole(ctx, claims, "PATCH", path, "merchant-role-change-3", strconv.FormatUint(merchantUserID, 10), MerchantUserRoleReq{RoleCode: "super_admin"}); problem.FromError(err).ErrorCode != "MERCHANT_ROLE_INVALID" {
		t.Fatalf("admin role assigned to merchant user: %v", err)
	}

	shopClaims := &auth.Claims{AccountType: "admin", AdminUserID: "9001", Permissions: []string{"merchant_user:authorize_shop"}}
	shopPath := "/api/v1/admin/merchant-users/:id/shops"
	if _, err := service.AuthorizeShops(ctx, shopClaims, "PUT", shopPath, "merchant-shop-change-1", strconv.FormatUint(merchantUserID, 10), ShopAuthorizationReq{ShopIDs: []string{strconv.FormatUint(shopID, 10)}}); err != nil {
		t.Fatalf("authorize own merchant shop: %v", err)
	}
	if err := tx.Table("accounts").Select("credential_version").Where("id=?", accountID).Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("shop authorization did not invalidate old tokens, credential_version=%d", version)
	}
	if _, err := service.AuthorizeShops(ctx, shopClaims, "PUT", shopPath, "merchant-shop-change-2", strconv.FormatUint(merchantUserID, 10), ShopAuthorizationReq{ShopIDs: []string{strconv.FormatUint(foreignShopID, 10)}}); problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("cross-merchant shop authorization was accepted: %v", err)
	}
	var foreignGrants int64
	if err := tx.Table("merchant_user_shops").Where("merchant_user_id=? AND shop_id=? AND deleted_at IS NULL", merchantUserID, foreignShopID).Count(&foreignGrants).Error; err != nil {
		t.Fatal(err)
	}
	if foreignGrants != 0 {
		t.Fatal("cross-merchant authorization created a grant")
	}
}
