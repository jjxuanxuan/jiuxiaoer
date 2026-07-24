package app

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

var customerAuthTestIDs = snowflake.New(987)

// seedCustomerReadyForSMSLogin 准备首次微信登录成功并绑定手机号后的状态。
// 关注下游行为的集成测试随后可以使用短信作为老用户登录方式。
func seedCustomerReadyForSMSLogin(t *testing.T, db *gorm.DB, cfg config.Config, phone string) (uint64, uint64) {
	t.Helper()
	if db == nil {
		t.Fatal("database is required to seed customer auth state")
	}
	if cfg.WeChat.MiniAppID == "" {
		t.Fatal("wechat miniapp id is required to seed customer auth state")
	}

	accountID := customerAuthTestIDs.Next()
	customerID := customerAuthTestIDs.Next()
	phoneCopy := phone
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&auth.Account{
			ID: accountID, AccountType: "customer", Phone: &phoneCopy,
			Status: "active", CredentialVersion: 1,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&auth.Customer{
			ID: customerID, AccountID: accountID, Phone: phone, Status: "active",
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&auth.CustomerIdentity{
			ID: customerAuthTestIDs.Next(), CustomerID: customerID,
			Provider: "wechat_miniapp", AppID: cfg.WeChat.MiniAppID,
			ProviderSubject: fmt.Sprintf("test-openid-%d", customerID), Status: "active",
		}).Error; err != nil {
			return err
		}
		return tx.Create(&auth.Cart{ID: customerAuthTestIDs.Next(), CustomerID: customerID}).Error
	})
	if err != nil {
		t.Fatalf("seed customer auth state: %v", err)
	}
	return accountID, customerID
}
