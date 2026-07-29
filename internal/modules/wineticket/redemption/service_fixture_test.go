package redemption

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type redemptionTestCustomer struct {
	ID        uint64
	Status    string
	DeletedAt *time.Time
}

func (redemptionTestCustomer) TableName() string { return "customers" }

type redemptionTestRealname struct {
	CustomerID  uint64 `gorm:"primaryKey"`
	Status      string
	AdultResult string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

func (redemptionTestRealname) TableName() string { return "customer_realname_verifications" }

type redemptionTestVerificationRequest struct {
	ID               uint64
	CustomerID       uint64
	Status           string
	SessionExpiresAt *time.Time
}

func (redemptionTestVerificationRequest) TableName() string {
	return "identity_verification_requests"
}

type redemptionTestMerchant struct {
	ID           uint64
	Name         string
	Status       string
	ReviewStatus string
	DeletedAt    *time.Time
}

func (redemptionTestMerchant) TableName() string { return "merchants" }

type redemptionTestShop struct {
	ID             uint64
	MerchantID     uint64
	Name           string
	Status         string
	BusinessStatus string
	DeletedAt      *time.Time
}

func (redemptionTestShop) TableName() string { return "shops" }

type redemptionTestCategory struct {
	ID        uint64
	Status    string
	DeletedAt *time.Time
}

func (redemptionTestCategory) TableName() string { return "categories" }

type redemptionTestProduct struct {
	ID            uint64
	CategoryID    uint64
	Name          string
	BrandName     *string
	Spec          *string
	ImageURL      *string
	Status        string
	AgeRestricted bool
	DeletedAt     *time.Time
}

func (redemptionTestProduct) TableName() string { return "products" }

type redemptionTestShopProduct struct {
	ID         uint64
	MerchantID uint64
	ShopID     uint64
	ProductID  uint64
	Status     string
	DeletedAt  *time.Time
}

func (redemptionTestShopProduct) TableName() string { return "shop_products" }

type redemptionTestRenewal struct {
	ID     uint64
	LotID  uint64
	Status string
}

func (redemptionTestRenewal) TableName() string { return "wine_ticket_renewals" }

type redemptionTestOrder struct {
	ID                       uint64
	OrderNo                  string
	OrderType                string
	SettlementMode           string
	CustomerID               uint64
	MerchantID               uint64
	ShopID                   uint64
	Status                   string
	PayStatus                string
	DeliveryStatus           string
	GoodsAmount              int64
	DiscountAmount           int64
	DeliveryFeeAmount        int64
	PayableAmount            int64
	PaidAmount               int64
	Remark                   *string
	AddressSnapshot          datatypes.JSON
	DeliveryTimeSlotID       *uint64
	DeliveryTimeSlotSnapshot datatypes.JSON
	DeliveryPromiseSnapshot  datatypes.JSON
	ComplianceSnapshot       datatypes.JSON
	IdempotencyKey           *string
	ExpiresAt                *time.Time
	CancelSource             *string
	CancelReasonCode         *string
	Version                  int
	PaidAt                   *time.Time
	CancelledAt              *time.Time
	CompletedAt              *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
	DeletedAt                *time.Time
}

func (redemptionTestOrder) TableName() string { return "orders" }

type redemptionTestOrderItem struct {
	ID              uint64
	OrderID         uint64
	ShopProductID   uint64
	ProductID       uint64
	ProductSnapshot datatypes.JSON
	Quantity        int
	SalePriceAmount int64
	TotalAmount     int64
}

func (redemptionTestOrderItem) TableName() string { return "order_items" }

type redemptionTestOrderLog struct {
	ID         uint64
	OrderID    uint64
	ActorType  string
	ActorID    uint64
	Action     string
	FromStatus *string
	ToStatus   *string
	Remark     *string
	RequestID  *string
	CreatedAt  time.Time
}

func (redemptionTestOrderLog) TableName() string { return "order_logs" }

type redemptionTestStockRecord struct {
	ID                 uint64
	ShopProductID      uint64
	ShopID             uint64
	ProductID          uint64
	ChangeType         string
	QuantityDelta      int
	BeforeAvailableQty int
	AfterAvailableQty  int
	TotalQuantityDelta int
	BeforeTotalQty     int
	AfterTotalQty      int
	SourceType         string
	SourceID           uint64
	IdempotencyKey     *string
	BusinessActionKey  *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (redemptionTestStockRecord) TableName() string { return "stock_records" }

type redemptionTestPayment struct {
	ID uint64
}

func (redemptionTestPayment) TableName() string { return "payments" }

type redemptionTestDelivery struct {
	ID                uint64
	OrderID           uint64
	ShopID            uint64
	RiderID           *uint64
	Status            string
	AssignmentVersion uint
	DispatchStatus    string
	PickupReadyStatus string
	RecipientSnapshot datatypes.JSON
	ScheduledStartAt  time.Time
	ScheduledEndAt    time.Time
	NotBeforeAt       time.Time
	AcceptedAt        *time.Time
	PickedUpAt        *time.Time
	CompletedAt       *time.Time
	CancelledAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

func (redemptionTestDelivery) TableName() string { return "delivery_orders" }

type redemptionTestAfterSale struct {
	ID        uint64
	OrderID   uint64
	DeletedAt *time.Time
}

func (redemptionTestAfterSale) TableName() string { return "after_sales" }

type redemptionTestDeliveryReturn struct {
	ID      uint64
	OrderID uint64
}

func (redemptionTestDeliveryReturn) TableName() string { return "delivery_returns" }

type redemptionTestAudit struct {
	ID           uint64
	ActorType    string
	ActorID      uint64
	AccountID    *uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	RequestID    *string
	IPHash       *string
	UserAgent    *string
	CreatedAt    time.Time
}

func (redemptionTestAudit) TableName() string { return "audit_logs" }

type redemptionTestOutbox struct {
	ID            uint64
	EventID       string
	EventType     string
	EventVersion  uint
	SpecVersion   string
	AggregateType string
	AggregateID   uint64
	Producer      *string
	Payload       datatypes.JSON
	Status        string
	RetryCount    int
	RequestID     *string
	CreatedAt     time.Time
}

func (redemptionTestOutbox) TableName() string { return "outbox_events" }

type redemptionTestDispatch struct {
	failEnsure bool
}

func (d *redemptionTestDispatch) EnsureRedemptionTaskTx(
	ctx context.Context,
	tx *gorm.DB,
	input RedemptionDispatchCreateInput,
) (RedemptionDispatchState, error) {
	if d.failEnsure {
		return RedemptionDispatchState{}, errors.New("injected dispatch failure")
	}
	row := redemptionTestDelivery{
		ID: input.OrderID + 900000, OrderID: input.OrderID, ShopID: input.ShopID,
		Status: "pending_assign", AssignmentVersion: 1, DispatchStatus: "pending",
		PickupReadyStatus: "waiting_store", RecipientSnapshot: cloneJSON(input.AddressSnapshot),
		ScheduledStartAt: input.ScheduledStartAt, ScheduledEndAt: input.ScheduledEndAt,
		NotBeforeAt: input.NotBeforeAt, CreatedAt: input.NotBeforeAt, UpdatedAt: input.NotBeforeAt,
	}
	if err := tx.WithContext(ctx).Create(&row).Error; err != nil {
		return RedemptionDispatchState{}, err
	}
	return RedemptionDispatchState{
		DeliveryOrderID: row.ID, OrderID: row.OrderID, Status: row.Status,
		DispatchStatus: row.DispatchStatus,
	}, nil
}

func (d *redemptionTestDispatch) LockCancellationPrefixTx(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) (RedemptionDispatchState, error) {
	var row redemptionTestDelivery
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("order_id = ? AND deleted_at IS NULL", orderID).
		Take(&row).Error
	if err != nil {
		return RedemptionDispatchState{}, err
	}
	return RedemptionDispatchState{
		DeliveryOrderID: row.ID, OrderID: row.OrderID, Status: row.Status,
		DispatchStatus: row.DispatchStatus, RiderID: row.RiderID,
		AcceptedAt: row.AcceptedAt, PickedUpAt: row.PickedUpAt,
		CompletedAt: row.CompletedAt, CancelledAt: row.CancelledAt,
	}, nil
}

func (d *redemptionTestDispatch) ApplyCancellationTx(
	ctx context.Context,
	tx *gorm.DB,
	input RedemptionDispatchCancelInput,
) error {
	result := tx.WithContext(ctx).Model(&redemptionTestDelivery{}).
		Where("id = ? AND order_id = ?", input.State.DeliveryOrderID, input.State.OrderID).
		Updates(map[string]any{
			"status": "cancelled", "dispatch_status": "cancelled",
			"cancelled_at": input.CancelledAt, "updated_at": input.CancelledAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("delivery cancellation lost its locked row")
	}
	return nil
}

type redemptionFixture struct {
	db          *gorm.DB
	service     *RedemptionService
	dispatch    *redemptionTestDispatch
	claims      *auth.Claims
	now         time.Time
	customerID  uint64
	productID   uint64
	addressID   uint64
	slotID      uint64
	stockID     uint64
	shopID      uint64
	shopProduct uint64
	lotEarlyID  uint64
	lotLaterID  uint64
	lotTooSoon  uint64
	otherLotID  uint64
	lotExpiry   time.Time
	slotStart   time.Time
	slotEnd     time.Time
}

func newRedemptionFixture(t *testing.T) redemptionFixture {
	t.Helper()
	dsn := testutil.UniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	models := []any{
		&redemptionTestCustomer{}, &redemptionTestRealname{},
		&redemptionTestVerificationRequest{}, &redemptionAddressRecord{},
		&redemptionTestMerchant{}, &redemptionTestShop{}, &redemptionTestCategory{},
		&redemptionTestProduct{}, &redemptionTestShopProduct{},
		&PhysicalStock{}, &redemptionTestStockRecord{},
		&DeliveryTimeSlot{}, &core.Lot{}, &core.Transaction{}, &redemptionTestRenewal{},
		&Redemption{}, &RedemptionAllocation{}, &redemptionTestOrder{},
		&redemptionTestOrderItem{}, &redemptionTestOrderLog{},
		&redemptionTestPayment{}, &redemptionTestDelivery{},
		&redemptionTestAfterSale{}, &redemptionTestDeliveryReturn{},
		&idempotency.Record{}, &redemptionTestAudit{}, &redemptionTestOutbox{},
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("migrate redemption fixture: %v", err)
	}

	now := time.Date(2026, 7, 27, 9, 0, 0, 0, shanghaiLocation)
	slotStart := time.Date(2026, 7, 28, 14, 0, 0, 0, shanghaiLocation)
	slotEnd := time.Date(2026, 7, 28, 16, 0, 0, 0, shanghaiLocation)
	customerID := uint64(7001)
	productID := uint64(3001)
	addressID := uint64(7101)
	merchantID := uint64(1001)
	shopID := uint64(2001)
	shopProductID := uint64(4001)
	stockID := uint64(5001)
	slotID := uint64(6001)
	categoryID := uint64(8001)
	cityCode := "310100"
	brand := "测试品牌"
	spec := "500ml"
	image := "https://example.invalid/wine.png"
	if err := db.Create(&redemptionTestCustomer{ID: customerID, Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemptionTestRealname{
		CustomerID: customerID, Status: "verified", AdultResult: "adult",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemptionAddressRecord{
		ID: addressID, CustomerID: customerID, ContactName: "张三",
		ContactPhone: "13812345678", Province: "上海市", City: "上海市",
		CityCode: &cityCode, District: "浦东新区", AddressDetail: "世纪大道 1 号",
		CoordinateSystem: "gcj02", LocationSource: "legacy", GeocodeStatus: "verified",
		Version: 3,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemptionTestMerchant{
		ID: merchantID, Name: "发行商一", Status: "active", ReviewStatus: "approved",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemptionTestShop{
		ID: shopID, MerchantID: merchantID, Name: "浦东履约店",
		Status: "active", BusinessStatus: "open",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemptionTestCategory{ID: categoryID, Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemptionTestProduct{
		ID: productID, CategoryID: categoryID, Name: "测试白酒",
		BrandName: &brand, Spec: &spec, ImageURL: &image,
		Status: "on_sale", AgeRestricted: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemptionTestShopProduct{
		ID: shopProductID, MerchantID: merchantID, ShopID: shopID,
		ProductID: productID, Status: "on_sale",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&PhysicalStock{
		ID: stockID, ShopProductID: shopProductID, ShopID: shopID,
		ProductID: productID, AvailableQty: 10, ReservedQty: 0,
		LockedQty: 0, Version: 1, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&DeliveryTimeSlot{
		ID: slotID, ShopID: shopID, ServiceDate: slotStart,
		StartTime: "14:00:00", EndTime: "16:00:00",
		CutoffAt: slotStart.Add(-2 * time.Hour), CapacityOrders: 2,
		ReservedOrders: 0, Status: "open", Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	lotTooSoon := uint64(9001)
	lotEarly := uint64(9002)
	lotLater := uint64(9003)
	otherLot := uint64(9004)
	lotExpiry := time.Date(2026, 8, 1, 0, 0, 0, 0, shanghaiLocation)
	for _, lot := range []core.Lot{
		redemptionFixtureLot(
			lotTooSoon, "WTL_TOO_SOON", customerID, merchantID, productID, cityCode,
			5, slotEnd.Add(-time.Hour), now,
		),
		redemptionFixtureLot(
			lotEarly, "WTL_FEFO_EARLY", customerID, merchantID, productID, cityCode,
			2, lotExpiry, now,
		),
		redemptionFixtureLot(
			lotLater, "WTL_FEFO_LATER", customerID, merchantID, productID, cityCode,
			3, lotExpiry.AddDate(0, 0, 1), now,
		),
		redemptionFixtureLot(
			otherLot, "WTL_OTHER_ISSUER", customerID, 1002, productID, cityCode,
			10, lotExpiry, now,
		),
	} {
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
	}

	dispatch := &redemptionTestDispatch{}
	service := NewRedemptionService(db, snowflake.New(37)).
		WithDispatch(dispatch).
		WithNow(func() time.Time { return now })
	claims := &auth.Claims{
		AccountType: "customer", CustomerID: fmt.Sprint(customerID),
		Permissions: []string{
			"wine_ticket_slot:list", "wine_ticket_redemption:create",
			"wine_ticket_redemption:list", "wine_ticket_redemption:view",
			"wine_ticket_redemption:cancel",
		},
	}
	return redemptionFixture{
		db: db, service: service, dispatch: dispatch, claims: claims, now: now,
		customerID: customerID, productID: productID, addressID: addressID,
		slotID: slotID, stockID: stockID, shopID: shopID, shopProduct: shopProductID,
		lotEarlyID: lotEarly, lotLaterID: lotLater, lotTooSoon: lotTooSoon,
		otherLotID: otherLot, lotExpiry: lotExpiry, slotStart: slotStart, slotEnd: slotEnd,
	}
}

func redemptionFixtureLot(
	id uint64,
	lotNo string,
	customerID uint64,
	merchantID uint64,
	productID uint64,
	cityCode string,
	quantity uint,
	expiresAt time.Time,
	now time.Time,
) core.Lot {
	return core.Lot{
		ID: id, LotNo: lotNo, OwnerCustomerID: customerID, PurchaseID: id + 10000,
		SourceType: LotSourcePurchase, IssuerMerchantID: merchantID,
		ProductID: productID, RedeemCityCode: cityCode,
		TotalQuantity: quantity, AvailableQuantity: quantity,
		OriginalExpiresAt: expiresAt, ExpiresAt: expiresAt, ExpiryChangedAt: now,
		Status: LotStatusActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func (fx redemptionFixture) createRequest(quantity uint) RedemptionCreateRequest {
	return RedemptionCreateRequest{
		ProductID: fmt.Sprint(fx.productID), Quantity: quantity,
		AddressID: fmt.Sprint(fx.addressID), AddressVersion: 3,
		DeliveryTimeSlotID: fmt.Sprint(fx.slotID),
	}
}
