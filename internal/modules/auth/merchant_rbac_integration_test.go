package auth

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestMerchantLeastPrivilegeRBACMySQL(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run merchant RBAC integration test")
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

	ids := snowflake.New(723)
	accountID, merchantUserID, merchantID, shopID := ids.Next(), ids.Next(), ids.Next(), ids.Next()
	if err := tx.Exec(`INSERT INTO accounts (id,account_type,username,status,credential_version) VALUES (?,'merchant',?,'active',1)`, accountID, "rbac_"+idString(accountID)).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`INSERT INTO merchant_users (id,account_id,merchant_id,role_id,name,status) VALUES (?,?,?,?,?,'active')`, merchantUserID, accountID, merchantID, uint64(1007), "RBAC integration").Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`INSERT INTO merchant_user_shops (id,merchant_user_id,merchant_id,shop_id) VALUES (?,?,?,?)`, ids.Next(), merchantUserID, merchantID, shopID).Error; err != nil {
		t.Fatal(err)
	}

	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	service := NewService(cfg, tx, redisClient, ids)
	account := Account{ID: accountID, AccountType: "merchant", Status: "active", CredentialVersion: 1}
	tests := []struct {
		roleID      uint64
		roleCode    string
		mustHave    string
		mustNotHave string
	}{
		{1007, "merchant_order_operator", "store_order:accept", "inventory:adjust"},
		{1008, "merchant_inventory_clerk", "inventory:adjust", "store_order:accept"},
		{1006, "merchant_owner", "inventory:adjust", ""},
	}
	for _, tt := range tests {
		if err := tx.Exec("UPDATE merchant_users SET role_id=? WHERE id=?", tt.roleID, merchantUserID).Error; err != nil {
			t.Fatal(err)
		}
		identity, err := service.identityForAccount(ctx, account)
		if err != nil {
			t.Fatalf("load %s: %v", tt.roleCode, err)
		}
		if identity.RoleCode != tt.roleCode || !hasPermission(identity.Permissions, tt.mustHave) {
			t.Fatalf("role %s permission snapshot=%v", identity.RoleCode, identity.Permissions)
		}
		if tt.mustNotHave != "" && hasPermission(identity.Permissions, tt.mustNotHave) {
			t.Fatalf("role %s unexpectedly has %s", tt.roleCode, tt.mustNotHave)
		}
		if tt.roleID == 1006 {
			if len(identity.Permissions) != 25 {
				t.Fatalf("merchant owner permission count=%d, want 25: %v", len(identity.Permissions), identity.Permissions)
			}
			for _, permission := range []string{
				"store_order:list", "store_order:view", "store_order:accept", "store_order:prepare",
				"shop_product:list", "shop_product:create", "shop_product:update", "shop:business_status",
				"inventory:view", "inventory:adjust",
				"after_sale:list_shop", "after_sale:view_shop", "after_sale:review_shop", "after_sale:receive_return", "after_sale:create_replacement",
				"print_setting:view_shop", "print_setting:update_shop", "print_setting:test_shop",
				"print_task:list_shop", "print_task:reprint_shop", "delivery_verification:view_shop",
				"delivery_incident:view_shop", "delivery_return:list_shop", "delivery_return:view_shop", "delivery_return:receive_shop",
			} {
				if !hasPermission(identity.Permissions, permission) {
					t.Fatalf("merchant owner missing %s: %v", permission, identity.Permissions)
				}
			}
		}
	}

	// Role/shop scope is embedded in the token. Advancing credential_version in
	// the same transaction as a role change must revoke the already-issued token.
	if err := tx.Exec("UPDATE merchant_users SET role_id=1006 WHERE id=?", merchantUserID).Error; err != nil {
		t.Fatal(err)
	}
	ownerIdentity, err := service.identityForAccount(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := service.issueResponse(ctx, ownerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyAccess(ctx, tokens.AccessToken); err != nil {
		t.Fatalf("fresh role snapshot rejected: %v", err)
	}
	if err := tx.Exec(`UPDATE merchant_users SET role_id=1008 WHERE id=?`, merchantUserID).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`UPDATE accounts SET credential_version=credential_version+1,token_invalid_before=NOW(3) WHERE id=?`, accountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyAccess(ctx, tokens.AccessToken); problem.FromError(err).ErrorCode != "AUTH_SESSION_REVOKED" {
		t.Fatalf("old merchant permission snapshot remained usable: %v", err)
	}
}
