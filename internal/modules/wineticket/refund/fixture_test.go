package refund

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type refundTestProduct struct {
	ID        uint64
	Name      string
	BrandName *string
	Spec      *string
	ImageURL  *string
	Status    string
	DeletedAt *time.Time
}

func (refundTestProduct) TableName() string { return "products" }

type refundTestCustomer struct {
	ID        uint64 `gorm:"primaryKey"`
	Phone     string
	Status    string
	DeletedAt *time.Time
}

func (refundTestCustomer) TableName() string { return "customers" }

type refundTestRealname struct {
	CustomerID  uint64 `gorm:"primaryKey"`
	Status      string
	AdultResult string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

func (refundTestRealname) TableName() string {
	return "customer_realname_verifications"
}

type refundTestAudit struct {
	ID           uint64 `gorm:"primaryKey"`
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	BeforeStatus *string
	AfterStatus  *string
	Version      uint64
	RequestID    *string
	IPHash       *string
	UserAgent    *string
	CreatedAt    time.Time
}

func (refundTestAudit) TableName() string { return "audit_logs" }

type refundTestOutbox struct {
	ID            uint64 `gorm:"primaryKey"`
	EventID       string
	EventType     string
	EventVersion  uint
	SpecVersion   string
	AggregateType string
	AggregateID   uint64
	Producer      *string
	Payload       datatypes.JSON
	Status        string
	RetryCount    uint
	RequestID     *string
	CreatedAt     time.Time
}

func (refundTestOutbox) TableName() string { return "outbox_events" }

const (
	GiftStatusCancelled          = "cancelled"
	GiftAllocationStatusRestored = "restored"
)

func newRefundTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")),
		&gorm.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&purchasedomain.Purchase{},
		&core.Lot{},
		&core.Transaction{},
		&redemption.RedemptionAllocation{},
		&gift.GiftAllocation{},
		&RefundAllocation{},
		&renewal.Renewal{},
		&gift.Gift{},
		&WineTicketRefund{},
		&sharedrefund.Row{},
		&order.Payment{},
		&refundTestProduct{},
		&refundTestCustomer{},
		&refundTestRealname{},
		&refundTestAudit{},
		&refundTestOutbox{},
		&idempotency.Record{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE payments ADD COLUMN deleted_at datetime").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func seedRefundPurchase(
	t *testing.T,
	db *gorm.DB,
	now time.Time,
	purchaseID uint64,
	customerID uint64,
	packageQuantity uint,
	bottlesPerPackage uint,
) purchasedomain.Purchase {
	t.Helper()
	refundPolicy := datatypes.JSON(`{"schema_version":1,"enabled":true,"window_hours":168,"require_never_used":true,"fee_amount":0}`)
	renewalPolicy := datatypes.JSON(`{"schema_version":1,"enabled":true,"extension_days":30,"max_count":2,"grace_days":0,"fee_amount":990}`)
	productID := uint64(401)
	if err := db.FirstOrCreate(
		&refundTestProduct{ID: productID},
		&refundTestProduct{ID: productID, Name: "典藏干红", Status: "on_sale"},
	).Error; err != nil {
		t.Fatal(err)
	}
	packageID := purchaseID + 10_000
	packageNo := "WTP" + idString(purchaseID)
	snapshot, _ := json.Marshal(map[string]any{
		"schema_version":             1,
		"package_no":                 packageNo,
		"package_code":               "STOCKPILE_A",
		"package_name":               "典藏囤酒套餐",
		"package_type":               "stockpile",
		"package_version":            1,
		"validity_days":              365,
		"bottle_quantity":            bottlesPerPackage,
		"unit_price_amount":          1000,
		"issuer_merchant_id":         "1",
		"settlement_shop_id":         "2",
		"settlement_shop_product_id": "3",
		"redeem_city_code":           "310100",
		"product":                    map[string]any{"product_id": "401", "name": "典藏干红"},
	})
	purchase := purchasedomain.Purchase{
		ID: purchaseID, PurchaseNo: "WTPU" + idString(purchaseID),
		CustomerID: customerID, PackageID: packageID, PackageVersion: 1,
		PaymentID: purchaseID + 20_000, IssuerMerchantID: 1,
		SettlementShopID: 2, SettlementShopProductID: 3, ProductID: productID,
		RedeemCityCode: "310100", PackageQuantity: packageQuantity,
		BottleQuantityPerPackage: bottlesPerPackage,
		TotalBottleQuantity:      packageQuantity * bottlesPerPackage,
		UnitPriceAmount:          1000,
		PayableAmount:            int64(packageQuantity) * 1000,
		Currency:                 "CNY",
		PackageSnapshot:          datatypes.JSON(snapshot),
		RefundPolicySnapshot:     refundPolicy,
		RenewalPolicySnapshot:    renewalPolicy,
		Status:                   purchasedomain.PurchaseStatusPendingPayment,
		Version:                  1,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	if err := db.Create(&purchase).Error; err != nil {
		t.Fatal(err)
	}
	bizType, bizID := purchasedomain.PurchasePaymentBusiness, purchase.ID
	if err := db.Create(&order.Payment{
		ID: purchase.PaymentID, PaymentNo: "PAY-" + idString(purchase.ID),
		BizType: &bizType, BizID: &bizID, CustomerID: customerID,
		Channel: "wechat_miniapp", Provider: "wechat", Status: "pending",
		Amount: purchase.PayableAmount, Currency: "CNY",
		ClientPayload: datatypes.JSON(`{"timeStamp":"1","nonceStr":"n","package":"prepay_id=x","signType":"RSA","paySign":"s"}`),
		CreatedAt:     now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return purchase
}

func customerClaimsFor(customerID uint64, permissions ...string) *auth.Claims {
	return &auth.Claims{
		AccountType: "customer",
		CustomerID:  strconv.FormatUint(customerID, 10),
		Permissions: permissions,
	}
}
