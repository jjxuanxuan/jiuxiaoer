package purchase

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type purchaseRefundAllocationGuard struct {
	ID                 uint64
	WineTicketRefundID uint64
	LotID              uint64
	Quantity           uint
	SourceExpiresAt    time.Time
	Status             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (purchaseRefundAllocationGuard) TableName() string {
	return "wine_ticket_refund_allocations"
}

type customerAssetMerchant struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string
}

func (customerAssetMerchant) TableName() string { return "merchants" }

type customerAssetOutbox struct {
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

func (customerAssetOutbox) TableName() string { return "outbox_events" }

type customerAssetCustomer struct {
	ID        uint64 `gorm:"primaryKey"`
	Phone     string
	Status    string
	DeletedAt *time.Time
}

func (customerAssetCustomer) TableName() string { return "customers" }

type customerAssetIdentity struct {
	ID              uint64 `gorm:"primaryKey"`
	CustomerID      uint64
	Provider        string
	AppID           string `gorm:"column:app_id"`
	ProviderSubject string
	Status          string
	DeletedAt       *time.Time
}

func (customerAssetIdentity) TableName() string { return "customer_identities" }

type customerAssetRealname struct {
	CustomerID  uint64 `gorm:"primaryKey"`
	Status      string
	AdultResult string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

func (customerAssetRealname) TableName() string {
	return "customer_realname_verifications"
}

type customerAssetVerificationRequest struct {
	ID               uint64 `gorm:"primaryKey"`
	CustomerID       uint64
	Status           string
	SessionExpiresAt *time.Time
}

func (customerAssetVerificationRequest) TableName() string {
	return "identity_verification_requests"
}

type packageTestProduct struct {
	ID            uint64 `gorm:"primaryKey"`
	CategoryID    uint64
	Name          string
	BrandName     *string
	Spec          *string
	ImageURL      *string
	Status        string
	AgeRestricted bool
	DeletedAt     *time.Time
}

func (packageTestProduct) TableName() string { return "products" }

type packageTestAudit struct {
	ID           uint64 `gorm:"primaryKey"`
	AccountID    *uint64
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	BeforeData   []byte
	AfterData    []byte
	Result       string
	ErrorCode    *string
	BeforeStatus *string
	AfterStatus  *string
	Version      uint64
	RequestID    *string
	IPHash       *string
	UserAgent    *string
	CreatedAt    time.Time
}

func (packageTestAudit) TableName() string { return "audit_logs" }

func newCustomerAssetTestService(
	t *testing.T,
) (*Service, *gorm.DB, time.Time) {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")),
		&gorm.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&catalog.Package{},
		&PurchaseQuota{},
		&Purchase{},
		&core.Lot{},
		&core.Transaction{},
		&redemption.RedemptionAllocation{},
		&gift.GiftAllocation{},
		&purchaseRefundAllocationGuard{},
		&renewal.Renewal{},
		&redemption.Redemption{},
		&gift.Gift{},
		&issuanceCompensationRefund{},
		&commonRefundRow{},
		&order.Payment{},
		&packageTestProduct{},
		&customerAssetMerchant{},
		&packageTestAudit{},
		&customerAssetOutbox{},
		&customerAssetCustomer{},
		&customerAssetIdentity{},
		&customerAssetRealname{},
		&customerAssetVerificationRequest{},
		&idempotency.Record{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		"ALTER TABLE payments ADD COLUMN deleted_at datetime",
	).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(
		2026,
		7,
		27,
		15,
		30,
		0,
		123000000,
		shanghaiLocation,
	)
	service := NewService(db, snowflake.New(87)).
		WithNow(func() time.Time { return now })
	if err := db.Create(&packageTestProduct{
		ID:     401,
		Name:   "典藏干红",
		Status: "on_sale",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&customerAssetMerchant{
		ID:   1,
		Name: "酒票发行商",
	}).Error; err != nil {
		t.Fatal(err)
	}
	return service, db, now
}

func seedCustomerAssetPurchase(
	t *testing.T,
	db *gorm.DB,
	now time.Time,
	purchaseID uint64,
	customerID uint64,
	packageQuantity uint,
	bottlesPerPackage uint,
) Purchase {
	t.Helper()
	refund := datatypes.JSON(
		`{"schema_version":1,"enabled":true,"window_hours":168,"require_never_used":true,"fee_amount":0}`,
	)
	renewal := datatypes.JSON(
		`{"schema_version":1,"enabled":true,"extension_days":30,"max_count":2,"grace_days":0,"fee_amount":990}`,
	)
	pkg := catalog.Package{
		ID:                      purchaseID + 10_000,
		PackageNo:               "WTP" + idString(purchaseID),
		PackageCode:             "STOCKPILE_A",
		PackageVersion:          1,
		IssuerMerchantID:        1,
		SettlementShopID:        2,
		SettlementShopProductID: 3,
		ProductID:               401,
		RedeemCityCode:          "310100",
		PackageType:             PackageTypeStockpile,
		Name:                    "典藏囤酒套餐",
		BottleQuantity:          bottlesPerPackage,
		SalePriceAmount:         1000,
		MinPurchaseQuantity:     1,
		MaxPurchaseQuantity:     100,
		ValidityDays:            365,
		RefundPolicy:            refund,
		RenewalPolicy:           renewal,
		DeliveryPolicy: datatypes.JSON(
			`{"schema_version":1,"delivery_fee_included":true,"dispatch_lead_minutes":120}`,
		),
		Status:    PackageStatusPublished,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(purchasePackageSnapshot{
		SchemaVersion:           1,
		PackageNo:               pkg.PackageNo,
		PackageCode:             pkg.PackageCode,
		PackageName:             pkg.Name,
		PackageType:             pkg.PackageType,
		PackageVersion:          1,
		ValidityDays:            365,
		BottleQuantity:          bottlesPerPackage,
		UnitPriceAmount:         1000,
		IssuerMerchantID:        "1",
		SettlementShopID:        "2",
		SettlementShopProductID: "3",
		RedeemCityCode:          "310100",
		Product: core.ProductSummaryDTO{
			ProductID: "401",
			Name:      "典藏干红",
		},
	})
	purchase := Purchase{
		ID:                       purchaseID,
		PurchaseNo:               "WTPU" + idString(purchaseID),
		CustomerID:               customerID,
		PackageID:                pkg.ID,
		PackageVersion:           1,
		PaymentID:                purchaseID + 20_000,
		IssuerMerchantID:         1,
		SettlementShopID:         2,
		SettlementShopProductID:  3,
		ProductID:                401,
		RedeemCityCode:           "310100",
		PackageQuantity:          packageQuantity,
		BottleQuantityPerPackage: bottlesPerPackage,
		TotalBottleQuantity:      packageQuantity * bottlesPerPackage,
		UnitPriceAmount:          1000,
		PayableAmount:            int64(packageQuantity) * 1000,
		Currency:                 "CNY",
		PackageSnapshot:          datatypes.JSON(snapshot),
		RefundPolicySnapshot:     refund,
		RenewalPolicySnapshot:    renewal,
		Status:                   PurchaseStatusPendingPayment,
		Version:                  1,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	if err := db.Create(&purchase).Error; err != nil {
		t.Fatal(err)
	}
	bizType, bizID := PurchasePaymentBusiness, purchase.ID
	if err := db.Create(&order.Payment{
		ID:         purchase.PaymentID,
		PaymentNo:  "PAY-" + idString(purchase.ID),
		BizType:    &bizType,
		BizID:      &bizID,
		CustomerID: customerID,
		Channel:    "wechat_miniapp",
		Provider:   "wechat",
		Status:     "pending",
		Amount:     purchase.PayableAmount,
		Currency:   "CNY",
		ClientPayload: datatypes.JSON(
			`{"timeStamp":"1","nonceStr":"n","package":"prepay_id=x","signType":"RSA","paySign":"s"}`,
		),
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return purchase
}

func customerClaimsFor(
	customerID uint64,
	permissions ...string,
) *auth.Claims {
	return &auth.Claims{
		AccountType: "customer",
		CustomerID:  idString(customerID),
		Permissions: permissions,
	}
}

type purchasePaymentProvider struct {
	state order.ProviderPaymentState
}

func (p *purchasePaymentProvider) Code() string { return "wechat" }

func (p *purchasePaymentProvider) Create(
	_ context.Context,
	input order.CreateProviderPaymentInput,
) (order.ProviderPaymentResult, error) {
	state := p.state
	if state.Status == "" {
		state = order.ProviderPaymentState{
			Status:          "PENDING",
			ProviderTradeNo: "WX-" + input.PaymentNo,
		}
	}
	return order.ProviderPaymentResult{
		ProviderTradeNo:  state.ProviderTradeNo,
		ProviderPrepayID: "prepay-" + input.PaymentNo,
		Status:           state.Status,
		RequestID:        state.RequestID,
		ClientPayload: map[string]any{
			"timeStamp": "1785000000",
			"nonceStr":  "purchase-nonce",
			"package":   "prepay_id=prepay-" + input.PaymentNo,
			"signType":  "RSA",
			"paySign":   "purchase-signature",
		},
	}, nil
}

func (p *purchasePaymentProvider) Query(
	context.Context,
	string,
) (order.ProviderPaymentState, error) {
	return p.state, nil
}

func (p *purchasePaymentProvider) Close(
	context.Context,
	string,
) (order.ProviderOperationResult, error) {
	return order.ProviderOperationResult{}, nil
}

func (p *purchasePaymentProvider) ParseCallback(
	context.Context,
	*http.Request,
) (order.PaymentCallbackEvent, error) {
	return order.PaymentCallbackEvent{}, nil
}

func (*purchasePaymentProvider) Shutdown() {}
