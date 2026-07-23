package provisioning

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestMerchantRoleValidationDefaultsOnlyInitialOwner(t *testing.T) {
	if got, err := normalizeMerchantRoleCode("", true); err != nil || got != MerchantRoleOwner {
		t.Fatalf("initial merchant role default = %q, err=%v", got, err)
	}
	if _, err := normalizeMerchantRoleCode("", false); problem.FromError(err).ErrorCode != "MERCHANT_ROLE_INVALID" {
		t.Fatalf("additional user without explicit role was accepted: %v", err)
	}
	for _, illegal := range []string{"super_admin", "operation", "merchant_unknown"} {
		if _, err := normalizeMerchantRoleCode(illegal, false); problem.FromError(err).ErrorCode != "MERCHANT_ROLE_INVALID" {
			t.Fatalf("illegal role %q was accepted: %v", illegal, err)
		}
	}
}

func TestMerchantRoleLookupRequiresActiveMerchantScope(t *testing.T) {
	db := provisioningRBACSQLite(t)
	if err := db.Exec(`CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL, scope TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO roles(id,code,scope,status) VALUES
		(1006,'merchant_owner','merchant','active'),
		(1001,'super_admin','all','active'),
		(1007,'merchant_order_operator','merchant','disabled')`).Error; err != nil {
		t.Fatal(err)
	}
	if id, err := merchantRoleID(context.Background(), db, MerchantRoleOwner); err != nil || id != 1006 {
		t.Fatalf("active merchant owner lookup id=%d err=%v", id, err)
	}
	for _, code := range []string{"super_admin", MerchantRoleOrderOperator} {
		if _, err := merchantRoleID(context.Background(), db, code); problem.FromError(err).ErrorCode != "MERCHANT_ROLE_INVALID" {
			t.Fatalf("non-merchant/inactive role %q was accepted: %v", code, err)
		}
	}
}

func TestMerchantShopScopeRejectsCrossMerchantAuthorization(t *testing.T) {
	db := provisioningRBACSQLite(t)
	if err := db.Exec(`CREATE TABLE shops (id INTEGER PRIMARY KEY, merchant_id INTEGER NOT NULL, deleted_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO shops(id,merchant_id) VALUES (11,1),(12,1),(21,2)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateMerchantShopScope(context.Background(), db, 1, []uint64{11, 12}); err != nil {
		t.Fatalf("same-merchant shops rejected: %v", err)
	}
	if err := validateMerchantShopScope(context.Background(), db, 1, []uint64{11, 21}); problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("cross-merchant shop authorization was accepted: %v", err)
	}
}

func TestRoleOrShopChangeInvalidatesExistingTokens(t *testing.T) {
	db := provisioningRBACSQLite(t)
	if err := db.Exec(`CREATE TABLE accounts (
		id INTEGER PRIMARY KEY, credential_version INTEGER NOT NULL,
		token_invalid_before DATETIME, updated_by INTEGER, deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO accounts(id,credential_version) VALUES (1,4)`).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := invalidateAccountTokens(db, 1, 99, now); err != nil {
		t.Fatalf("invalidate account tokens: %v", err)
	}
	var row struct {
		CredentialVersion  uint
		TokenInvalidBefore *time.Time
		UpdatedBy          uint64
	}
	if err := db.Table("accounts").Select("credential_version", "token_invalid_before", "updated_by").Where("id=1").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.CredentialVersion != 5 || row.TokenInvalidBefore == nil || row.UpdatedBy != 99 {
		t.Fatalf("token revocation state not advanced atomically: %+v", row)
	}
}

func provisioningRBACSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return db
}
