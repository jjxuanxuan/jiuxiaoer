package ops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type slotAdminTestAudit struct {
	ID           uint64 `gorm:"primaryKey;autoIncrement:false"`
	AccountID    *uint64
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	ShopID       *uint64
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

func (slotAdminTestAudit) TableName() string { return "audit_logs" }

type slotAdminTestOutbox struct {
	ID              uint64 `gorm:"primaryKey;autoIncrement:false"`
	EventID         string
	EventType       string
	EventVersion    uint
	SpecVersion     string
	AggregateType   string
	AggregateID     uint64
	Producer        string
	SchemaRef       string
	PartitionKey    string
	ReplayOfEventID string
	Payload         datatypes.JSON
	Status          string
	RetryCount      int
	NextRetryAt     *time.Time
	PublishedAt     *time.Time
	ExchangeName    *string
	RoutingKey      *string
	DispatchedAt    *time.Time
	RequestID       *string
	LockedBy        *string
	LockedUntil     *time.Time
	LastErrorCode   *string
	LastErrorDetail *string
	CreatedAt       time.Time
}

func (slotAdminTestOutbox) TableName() string { return "outbox_events" }

func TestSlotAdminCreateListIdempotencyOverlapAndScope(t *testing.T) {
	service, db, now := newSlotAdminTestService(t)
	seedSlotAdminShop(t, db, 201, 101, "310100")
	claims := slotAdminClaims(
		"wine_ticket_slot:list",
		"wine_ticket_slot:create",
	)
	request := validSlotAdminCreateRequest()

	created, err := service.Create(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"slot-create-0001",
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.ShopID != "201" ||
		created.IssuerMerchantID != "101" ||
		created.ServiceDate != "2026-07-28" ||
		created.ScheduledStartAt != "2026-07-28T10:00:00+08:00" ||
		created.ScheduledEndAt != "2026-07-28T12:00:00+08:00" ||
		created.CutoffAt != "2026-07-28T09:00:00+08:00" ||
		created.Status != DeliveryTimeSlotStatusOpen ||
		created.AvailabilityStatus != DeliveryTimeSlotStatusOpen ||
		created.ReservedOrders != 0 ||
		created.RemainingCapacity != request.CapacityOrders ||
		created.Version != 1 {
		t.Fatalf("unexpected created slot: %+v", created)
	}

	*now = time.Date(2026, 7, 28, 9, 30, 0, 0, shanghaiLocation)
	replayed, err := service.Create(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"slot-create-0001",
		request,
	)
	if err != nil {
		t.Fatalf("completed request must replay after cutoff: %v", err)
	}
	if replayed.SlotID != created.SlotID || replayed.Version != created.Version {
		t.Fatalf("replay mismatch: created=%+v replay=%+v", created, replayed)
	}
	*now = time.Date(2026, 7, 27, 15, 30, 0, 0, shanghaiLocation)

	overlap := request
	overlap.StartTime = "11:00:00"
	overlap.EndTime = "13:00:00"
	overlap.CutoffAt = "2026-07-28T10:30:00+08:00"
	_, err = service.Create(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"slot-create-overlap",
		overlap,
	)
	assertSlotAdminProblem(t, err, "WT_CONCURRENT_MODIFICATION")

	adjacent := request
	adjacent.StartTime = "12:00:00"
	adjacent.EndTime = "13:00:00"
	adjacent.CutoffAt = "2026-07-28T11:00:00+08:00"
	second, err := service.Create(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"slot-create-adjacent",
		adjacent,
	)
	if err != nil {
		t.Fatalf("adjacent window must not overlap: %v", err)
	}
	if second.SlotID == created.SlotID {
		t.Fatal("adjacent create reused slot id")
	}

	items, next, err := service.List(
		context.Background(),
		claims,
		pagination.Query{PageSize: 1},
		"201",
		"2026-07-28",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || next == "" {
		t.Fatalf("list items=%d next=%q", len(items), next)
	}

	scopedClaims := slotAdminClaims("wine_ticket_slot:list")
	scopedClaims.RoleCode = "operation"
	scopedClaims.AuthorizedShopIDs = []string{"202"}
	_, _, err = service.List(
		context.Background(),
		scopedClaims,
		pagination.Query{PageSize: 20},
		"201",
		"",
	)
	assertSlotAdminProblem(t, err, "PERM_FORBIDDEN")
	items, _, err = service.List(
		context.Background(),
		scopedClaims,
		pagination.Query{PageSize: 20},
		"",
		"",
	)
	if err != nil || len(items) != 0 {
		t.Fatalf("scoped list items=%d err=%v", len(items), err)
	}

	var slotCount, auditCount, outboxCount int64
	if err := db.Model(&redemption.DeliveryTimeSlot{}).Count(&slotCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&slotAdminTestAudit{}).Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&slotAdminTestOutbox{}).
		Where("event_type = ?", slotAdminChangedEvent).
		Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if slotCount != 2 || auditCount != 2 || outboxCount != 2 {
		t.Fatalf(
			"slot=%d audit=%d outbox=%d, want exactly two committed writes",
			slotCount,
			auditCount,
			outboxCount,
		)
	}
}

func TestSlotAdminUpdateProtectsReservationsCASAndCustomerSelection(t *testing.T) {
	service, db, now := newSlotAdminTestService(t)
	seedSlotAdminShop(t, db, 201, 101, "310100")
	claims := slotAdminClaims(
		"wine_ticket_slot:create",
		"wine_ticket_slot:update",
	)
	created, err := service.Create(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"slot-update-create",
		validSlotAdminCreateRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	slotID, err := parseExternalID(created.SlotID, "slot_id")
	if err != nil {
		t.Fatal(err)
	}
	scopedClaims := slotAdminClaims("wine_ticket_slot:update")
	scopedClaims.RoleCode = "operation"
	scopedClaims.AuthorizedShopIDs = []string{"202"}
	_, err = service.Update(
		context.Background(),
		scopedClaims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-scoped-not-found",
		created.SlotID,
		SlotAdminUpdateRequest{
			CapacityOrders:  4,
			Status:          DeliveryTimeSlotStatusClosed,
			ExpectedVersion: created.Version,
		},
	)
	assertSlotAdminProblem(t, err, "WT_SLOT_NOT_FOUND")
	if err := db.Model(&redemption.DeliveryTimeSlot{}).
		Where("id = ? AND version = ?", slotID, created.Version).
		Updates(map[string]any{
			"reserved_orders": 2,
			"version":         gorm.Expr("version + 1"),
		}).Error; err != nil {
		t.Fatal(err)
	}
	const reservedVersion = 2

	_, err = service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-capacity-small",
		created.SlotID,
		SlotAdminUpdateRequest{
			CapacityOrders:  1,
			Status:          DeliveryTimeSlotStatusOpen,
			ExpectedVersion: reservedVersion,
		},
	)
	assertSlotAdminProblem(t, err, "WT_CONCURRENT_MODIFICATION")

	changedCutoff := "2026-07-28T08:30:00+08:00"
	_, err = service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-cutoff-reserved",
		created.SlotID,
		SlotAdminUpdateRequest{
			CapacityOrders:  4,
			Status:          DeliveryTimeSlotStatusOpen,
			CutoffAt:        &changedCutoff,
			ExpectedVersion: reservedVersion,
		},
	)
	assertSlotAdminProblem(t, err, "WT_CONCURRENT_MODIFICATION")

	closed, err := service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-close-0001",
		created.SlotID,
		SlotAdminUpdateRequest{
			CapacityOrders:  2,
			Status:          DeliveryTimeSlotStatusClosed,
			ExpectedVersion: reservedVersion,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != DeliveryTimeSlotStatusClosed ||
		closed.AvailabilityStatus != DeliveryTimeSlotStatusClosed ||
		closed.ReservedOrders != 2 ||
		closed.CapacityOrders != 2 ||
		closed.Version != 3 {
		t.Fatalf("unexpected closed slot: %+v", closed)
	}
	var stored redemption.DeliveryTimeSlot
	if err := db.First(&stored, "id = ?", slotID).Error; err != nil {
		t.Fatal(err)
	}
	startAt, endAt, err := redemptionSlotWindow(
		stored.ServiceDate,
		stored.StartTime,
		stored.EndTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateLockedRedemptionSlot(
		stored,
		startAt,
		endAt,
		*now,
	); err == nil {
		t.Fatal("closed slot must not be customer-selectable")
	}
	if stored.ReservedOrders != 2 {
		t.Fatalf("closing slot damaged existing reservations: %+v", stored)
	}

	*now = time.Date(2026, 7, 28, 9, 30, 0, 0, shanghaiLocation)
	replay, err := service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-close-0001",
		created.SlotID,
		SlotAdminUpdateRequest{
			CapacityOrders:  2,
			Status:          DeliveryTimeSlotStatusClosed,
			ExpectedVersion: reservedVersion,
		},
	)
	if err != nil || replay.Version != closed.Version {
		t.Fatalf("closed update replay=%+v err=%v", replay, err)
	}

	_, err = service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-reopen-past-cutoff",
		created.SlotID,
		SlotAdminUpdateRequest{
			CapacityOrders:  3,
			Status:          DeliveryTimeSlotStatusOpen,
			ExpectedVersion: closed.Version,
		},
	)
	assertSlotAdminProblem(t, err, "WT_CONCURRENT_MODIFICATION")

	*now = time.Date(2026, 7, 27, 15, 30, 0, 0, shanghaiLocation)
	_, err = service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-stale-version",
		created.SlotID,
		SlotAdminUpdateRequest{
			CapacityOrders:  3,
			Status:          DeliveryTimeSlotStatusOpen,
			ExpectedVersion: reservedVersion,
		},
	)
	assertSlotAdminProblem(t, err, "WT_CONCURRENT_MODIFICATION")

	reopened, err := service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-reopen-valid",
		created.SlotID,
		SlotAdminUpdateRequest{
			CapacityOrders:  3,
			Status:          DeliveryTimeSlotStatusOpen,
			ExpectedVersion: closed.Version,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Status != DeliveryTimeSlotStatusOpen ||
		reopened.AvailabilityStatus != DeliveryTimeSlotStatusOpen ||
		reopened.ReservedOrders != 2 ||
		reopened.RemainingCapacity != 1 ||
		reopened.Version != closed.Version+1 {
		t.Fatalf("unexpected reopened slot: %+v", reopened)
	}
}

func TestSlotAdminReopenRejectsOverlapWithAnotherOpenWindow(t *testing.T) {
	service, db, now := newSlotAdminTestService(t)
	seedSlotAdminShop(t, db, 201, 101, "310100")
	claims := slotAdminClaims(
		"wine_ticket_slot:create",
		"wine_ticket_slot:update",
	)
	open, err := service.Create(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"slot-overlap-open",
		validSlotAdminCreateRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	serviceDate, err := parseSlotAdminServiceDate("2026-07-28")
	if err != nil {
		t.Fatal(err)
	}
	closedID := uint64(88002)
	if err := db.Create(&redemption.DeliveryTimeSlot{
		ID: closedID, ShopID: 201, ServiceDate: serviceDate,
		StartTime: "11:00:00", EndTime: "13:00:00",
		CutoffAt: time.Date(
			2026,
			7,
			28,
			10,
			30,
			0,
			0,
			shanghaiLocation,
		),
		CapacityOrders: 4, Status: DeliveryTimeSlotStatusClosed,
		Version: 1, CreatedAt: *now, UpdatedAt: *now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-overlap-reopen",
		idString(closedID),
		SlotAdminUpdateRequest{
			CapacityOrders:  4,
			Status:          DeliveryTimeSlotStatusOpen,
			ExpectedVersion: 1,
		},
	)
	assertSlotAdminProblem(t, err, "WT_CONCURRENT_MODIFICATION")

	var stored redemption.DeliveryTimeSlot
	if err := db.First(&stored, "id = ?", closedID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != DeliveryTimeSlotStatusClosed ||
		stored.Version != 1 ||
		open.Status != DeliveryTimeSlotStatusOpen {
		t.Fatalf("open=%+v rejected=%+v", open, stored)
	}
}

func TestSlotAdminValidationRejectsInvalidWindowShopAndIDs(t *testing.T) {
	service, db, _ := newSlotAdminTestService(t)
	seedSlotAdminShop(t, db, 201, 101, "310100")
	claims := slotAdminClaims("wine_ticket_slot:create", "wine_ticket_slot:update")

	tests := []struct {
		name   string
		mutate func(*SlotAdminCreateRequest)
	}{
		{
			name: "leading zero shop",
			mutate: func(request *SlotAdminCreateRequest) {
				request.ShopID = "0201"
			},
		},
		{
			name: "spaced shop",
			mutate: func(request *SlotAdminCreateRequest) {
				request.ShopID = " 201"
			},
		},
		{
			name: "invalid date",
			mutate: func(request *SlotAdminCreateRequest) {
				request.ServiceDate = "2026-02-30"
			},
		},
		{
			name: "noncanonical clock",
			mutate: func(request *SlotAdminCreateRequest) {
				request.StartTime = "10:00"
			},
		},
		{
			name: "reversed clock",
			mutate: func(request *SlotAdminCreateRequest) {
				request.StartTime = "12:00:00"
				request.EndTime = "10:00:00"
			},
		},
		{
			name: "cutoff without timezone",
			mutate: func(request *SlotAdminCreateRequest) {
				request.CutoffAt = "2026-07-28T09:00:00"
			},
		},
		{
			name: "cutoff after start",
			mutate: func(request *SlotAdminCreateRequest) {
				request.CutoffAt = "2026-07-28T10:00:00+08:00"
			},
		},
		{
			name: "zero capacity",
			mutate: func(request *SlotAdminCreateRequest) {
				request.CapacityOrders = 0
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validSlotAdminCreateRequest()
			test.mutate(&request)
			_, err := service.Create(
				context.Background(),
				claims,
				http.MethodPost,
				"/api/v1/admin/wine-tickets/delivery-time-slots",
				"slot-invalid-"+leftPadSlotAdminIndex(index),
				request,
			)
			assertSlotAdminProblem(t, err, "VALIDATION_FAILED")
		})
	}
	if strconv.IntSize == 64 {
		tooLarge := slotAdminMaxUint32 + 1
		request := validSlotAdminCreateRequest()
		request.CapacityOrders = uint(tooLarge)
		_, err := service.Create(
			context.Background(),
			claims,
			http.MethodPost,
			"/api/v1/admin/wine-tickets/delivery-time-slots",
			"slot-invalid-capacity-max",
			request,
		)
		assertSlotAdminProblem(t, err, "VALIDATION_FAILED")
	}

	cityCode := "bad"
	if err := db.Model(&slotAdminShop{}).
		Where("id = ?", 201).
		Update("city_code", cityCode).Error; err != nil {
		t.Fatal(err)
	}
	_, err := service.Create(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"slot-invalid-shop",
		validSlotAdminCreateRequest(),
	)
	assertSlotAdminProblem(t, err, "VALIDATION_FAILED")

	_, err = service.Update(
		context.Background(),
		claims,
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"slot-invalid-id",
		"01",
		SlotAdminUpdateRequest{
			CapacityOrders:  1,
			Status:          DeliveryTimeSlotStatusOpen,
			ExpectedVersion: 1,
		},
	)
	assertSlotAdminProblem(t, err, "VALIDATION_FAILED")
}

func newSlotAdminTestService(
	t *testing.T,
) (*SlotAdminService, *gorm.DB, *time.Time) {
	t.Helper()
	dsn := uniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")
	db, err := gorm.Open(
		sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	// SQLite 不支持行级 FOR UPDATE。
	// 单个连接可在确定性单元测试中保持相同的串行化契约；
	// 生产环境的 MySQL 使用门店记录行锁。
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&redemption.DeliveryTimeSlot{},
		&slotAdminShop{},
		&slotAdminMerchant{},
		&idempotency.Record{},
		&slotAdminTestAudit{},
		&slotAdminTestOutbox{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		CREATE UNIQUE INDEX uk_slot_admin_test_idempotency
		ON idempotency_keys(actor_type, actor_id, path, key_hash)
	`).Error; err != nil {
		t.Fatal(err)
	}
	core := NewService(db, snowflake.New(331), nil)
	fixedNow := time.Date(
		2026,
		7,
		27,
		15,
		30,
		0,
		0,
		shanghaiLocation,
	)
	core.now = func() time.Time { return fixedNow }
	service := NewSlotAdminService(core).
		WithSlotAdminClock(func() time.Time { return fixedNow })
	return service, db, &fixedNow
}

func seedSlotAdminShop(
	t *testing.T,
	db *gorm.DB,
	shopID uint64,
	merchantID uint64,
	cityCode string,
) {
	t.Helper()
	if err := db.Create(&slotAdminMerchant{
		ID: merchantID, Name: "测试发行商",
		Status: "active", ReviewStatus: "approved",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&slotAdminShop{
		ID: shopID, MerchantID: merchantID, Name: "浦东履约店",
		CityCode: &cityCode, Status: "active", BusinessStatus: "open",
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func slotAdminClaims(permissions ...string) *auth.Claims {
	return &auth.Claims{
		AccountType: "admin",
		AdminUserID: "9001",
		RoleCode:    "super_admin",
		Permissions: permissions,
	}
}

func validSlotAdminCreateRequest() SlotAdminCreateRequest {
	return SlotAdminCreateRequest{
		ShopID: "201", ServiceDate: "2026-07-28",
		StartTime: "10:00:00", EndTime: "12:00:00",
		CutoffAt: "2026-07-28T01:00:00Z", CapacityOrders: 4,
	}
}

func leftPadSlotAdminIndex(value int) string {
	return fmt.Sprintf("%04d", value)
}

func assertSlotAdminProblem(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s", code)
	}
	var details *problem.Details
	if !errors.As(err, &details) || details.ErrorCode != code {
		t.Fatalf("error=%v, want problem code %s", err, code)
	}
}
