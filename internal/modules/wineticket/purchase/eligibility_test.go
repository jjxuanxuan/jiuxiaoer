package purchase

import (
	"context"
	"testing"
	"time"
)

func TestPurchaseEligibilityRequiresOneActiveAdultMiniAppIdentity(t *testing.T) {
	service, db, now := newCustomerAssetTestService(t)
	expiresAt := now.AddDate(1, 0, 0)
	for _, row := range []any{
		&customerAssetCustomer{
			ID:     88,
			Phone:  "13800000000",
			Status: "active",
		},
		&customerAssetIdentity{
			ID:              89,
			CustomerID:      88,
			Provider:        "wechat_miniapp",
			AppID:           "wx-test",
			ProviderSubject: "openid-88",
			Status:          "active",
		},
		&customerAssetRealname{
			CustomerID:  88,
			Status:      "verified",
			AdultResult: "adult",
			ExpiresAt:   &expiresAt,
		},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	row, err := service.repo.CustomerPurchaseEligibility(
		context.Background(),
		db,
		88,
		"wx-test",
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if row.OpenID != "openid-88" ||
		row.IdentityCount != 1 ||
		row.RealnameStatus == nil ||
		*row.RealnameStatus != "verified" ||
		row.AdultResult == nil ||
		*row.AdultResult != "adult" {
		t.Fatalf("eligibility projection=%+v", row)
	}
	if err := validatePurchaseEligibility(row); err != nil {
		t.Fatal(err)
	}
}

func TestPurchaseEligibilityRejectsExpiredAdultVerification(t *testing.T) {
	service, db, now := newCustomerAssetTestService(t)
	expiredAt := now.Add(-time.Millisecond)
	for _, row := range []any{
		&customerAssetCustomer{
			ID:     98,
			Phone:  "13900000000",
			Status: "active",
		},
		&customerAssetIdentity{
			ID:              99,
			CustomerID:      98,
			Provider:        "wechat_miniapp",
			AppID:           "wx-test",
			ProviderSubject: "openid-98",
			Status:          "active",
		},
		&customerAssetRealname{
			CustomerID:  98,
			Status:      "verified",
			AdultResult: "adult",
			ExpiresAt:   &expiredAt,
		},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	row, err := service.repo.CustomerPurchaseEligibility(
		context.Background(),
		db,
		98,
		"wx-test",
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePurchaseEligibility(row); err == nil {
		t.Fatal("expired adult verification must not satisfy purchase eligibility")
	}
}
