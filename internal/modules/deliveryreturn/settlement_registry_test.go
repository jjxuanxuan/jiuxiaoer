package deliveryreturn

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type stubReturnSettlementHandler struct {
	key            ReturnSettlementKey
	settlementType string
}

func (h stubReturnSettlementHandler) RoutingKey() ReturnSettlementKey { return h.key }
func (h stubReturnSettlementHandler) SettlementType() string          { return h.settlementType }

func (h stubReturnSettlementHandler) InitialBinding(
	context.Context,
	*gorm.DB,
	OrderRef,
) (ReturnSettlementBinding, error) {
	bizID := uint64(19)
	return ReturnSettlementBinding{SettlementType: h.settlementType, SettlementBizID: &bizID}, nil
}

func (h stubReturnSettlementHandler) Approve(
	context.Context,
	*gorm.DB,
	Return,
	DeliveryOrder,
	OrderRef,
	uint64,
	string,
) (ReturnSettlementApproval, error) {
	return ReturnSettlementApproval{}, nil
}

func (h stubReturnSettlementHandler) SettleReceived(
	context.Context,
	*gorm.DB,
	Return,
	AfterSale,
	OrderRef,
) (bool, error) {
	return true, nil
}

type stagedReceiveHandler struct {
	stubReturnSettlementHandler
	stages *[]string
}

func (h *stagedReceiveHandler) PrepareReceived(
	_ context.Context,
	tx *gorm.DB,
	_ Return,
	_ AfterSale,
	_ OrderRef,
) (ReturnSettlementReceivePlan, error) {
	*h.stages = append(*h.stages, "prepare")
	return &stagedReceivePlan{tx: tx, stages: h.stages}, nil
}

func (h *stagedReceiveHandler) SettleReceived(
	context.Context,
	*gorm.DB,
	Return,
	AfterSale,
	OrderRef,
) (bool, error) {
	*h.stages = append(*h.stages, "legacy_apply")
	return true, nil
}

type stagedReceivePlan struct {
	tx     *gorm.DB
	stages *[]string
}

func (p *stagedReceivePlan) ApplyReceived(
	_ context.Context,
	tx *gorm.DB,
	_ Return,
	_ AfterSale,
	_ OrderRef,
) (bool, error) {
	if tx != p.tx {
		return false, gorm.ErrInvalidTransaction
	}
	*p.stages = append(*p.stages, "prepared_apply")
	return true, nil
}

type legacyReceiveHandler struct {
	stubReturnSettlementHandler
	settleCalls int
}

func (h *legacyReceiveHandler) SettleReceived(
	context.Context,
	*gorm.DB,
	Return,
	AfterSale,
	OrderRef,
) (bool, error) {
	h.settleCalls++
	return true, nil
}

type mysqlLotReceiveHandler struct {
	stubReturnSettlementHandler
	lotID     uint64
	attempted chan struct{}
}

func (h *mysqlLotReceiveHandler) PrepareReceived(
	ctx context.Context,
	tx *gorm.DB,
	_ Return,
	_ AfterSale,
	_ OrderRef,
) (ReturnSettlementReceivePlan, error) {
	close(h.attempted)
	var locked struct{ ID uint64 }
	if err := tx.WithContext(ctx).
		Raw("SELECT id FROM wine_ticket_lots WHERE id = ? FOR UPDATE", h.lotID).
		Scan(&locked).Error; err != nil {
		return nil, err
	}
	if locked.ID != h.lotID {
		return nil, gorm.ErrRecordNotFound
	}
	stages := make([]string, 0)
	return &stagedReceivePlan{tx: tx, stages: &stages}, nil
}

func TestReturnSettlementRegistryRoutesByImmutableOrderMode(t *testing.T) {
	t.Parallel()
	registry := newReturnSettlementRegistry()
	handler := stubReturnSettlementHandler{
		key: ReturnSettlementKey{
			OrderType:      "wine_ticket_redemption",
			SettlementMode: "wine_ticket",
		},
		settlementType: SettlementWineTicketRestore,
	}
	if err := registry.register(handler); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	resolved, ok := registry.resolve(ReturnSettlementKey{
		OrderType:      " wine_ticket_redemption ",
		SettlementMode: "wine_ticket",
	})
	if !ok || resolved.SettlementType() != SettlementWineTicketRestore {
		t.Fatalf("unexpected route: handler=%T ok=%v", resolved, ok)
	}
	byType, ok := registry.resolveType(SettlementWineTicketRestore)
	if !ok || byType.RoutingKey() != handler.RoutingKey() {
		t.Fatalf("unexpected settlement-type route: handler=%T ok=%v", byType, ok)
	}
}

func TestPreparedReturnSettlementLocksBusinessRowsBeforeStockAndReusesPlan(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open(uniqueSQLiteMemoryDSN(t)),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&ProductStock{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE product_stocks ADD COLUMN deleted_at datetime").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&ProductStock{
		ID: 1, ShopProductID: 101, ShopID: 11, ProductID: 21, AvailableQty: 3,
	}).Error; err != nil {
		t.Fatal(err)
	}

	stages := make([]string, 0, 3)
	callbackName := "test:prepared-return-stock-lock-order"
	if err := db.Callback().Query().Before("gorm:query").Register(
		callbackName,
		func(callbackTx *gorm.DB) {
			if callbackTx.Statement.Table == "product_stocks" {
				stages = append(stages, "stock_lock")
			}
		},
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove(callbackName)
	})

	handler := &stagedReceiveHandler{
		stubReturnSettlementHandler: stubReturnSettlementHandler{
			key: ReturnSettlementKey{
				OrderType:      "wine_ticket_redemption",
				SettlementMode: "wine_ticket",
			},
			settlementType: SettlementWineTicketRestore,
		},
		stages: &stages,
	}
	service := &Service{repo: NewRepository(db)}
	plan, stocks, err := service.prepareReceivedSettlementAndLockStocks(
		t.Context(),
		db,
		Return{},
		AfterSale{},
		OrderRef{},
		handler,
		[]uint64{101},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(stocks) != 1 {
		t.Fatalf("locked stocks = %d, want 1", len(stocks))
	}
	ready, err := applyReturnSettlementReceived(
		t.Context(),
		db,
		handler,
		plan,
		Return{},
		AfterSale{},
		OrderRef{},
	)
	if err != nil || !ready {
		t.Fatalf("apply prepared settlement: ready=%v err=%v", ready, err)
	}
	if got, want := strings.Join(stages, ","), "prepare,stock_lock,prepared_apply"; got != want {
		t.Fatalf("receive lock stages = %q, want %q", got, want)
	}
}

func TestLegacyReturnSettlementKeepsDirectApplyPath(t *testing.T) {
	handler := &legacyReceiveHandler{
		stubReturnSettlementHandler: stubReturnSettlementHandler{
			key: ReturnSettlementKey{
				OrderType:      "retail",
				SettlementMode: "cash",
			},
			settlementType: SettlementRetailCashRefund,
		},
	}
	plan, err := prepareReturnSettlementReceived(
		t.Context(),
		nil,
		handler,
		Return{},
		AfterSale{},
		OrderRef{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		t.Fatalf("legacy handler unexpectedly prepared %T", plan)
	}
	ready, err := applyReturnSettlementReceived(
		t.Context(),
		nil,
		handler,
		plan,
		Return{},
		AfterSale{},
		OrderRef{},
	)
	if err != nil || !ready || handler.settleCalls != 1 {
		t.Fatalf(
			"legacy apply changed: ready=%v calls=%d err=%v",
			ready,
			handler.settleCalls,
			err,
		)
	}
}

func TestPreparedReturnSettlementMySQLAvoidsStockLotDeadlock(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run return lock-order acceptance")
	}
	cfg := config.Load()
	db, err := mysqlinfra.Open(
		t.Context(),
		cfg.MySQL,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	db = db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ids := snowflake.New(978)
	lotID, stockID, shopProductID := ids.Next(), ids.Next(), ids.Next()
	customerID, purchaseID := ids.Next(), ids.Next()
	merchantID, productID, shopID := ids.Next(), ids.Next(), ids.Next()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.Exec(`
		INSERT INTO wine_ticket_lots (
			id, lot_no, owner_customer_id, purchase_id, source_type,
			issuer_merchant_id, product_id, redeem_city_code,
			total_quantity, available_quantity, original_expires_at,
			expires_at, expiry_changed_at, status, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'purchase', ?, ?, '310100', 1, 1, ?, ?, ?, 'active', 1, ?, ?)
	`,
		lotID,
		"WTRL"+idString(lotID),
		customerID,
		purchaseID,
		merchantID,
		productID,
		now.Add(24*time.Hour),
		now.Add(24*time.Hour),
		now,
		now,
		now,
	).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO product_stocks (
			id, shop_product_id, shop_id, product_id, available_qty,
			reserved_qty, locked_qty, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, 1, 0, 0, 1, ?, ?)
	`, stockID, shopProductID, shopID, productID, now, now).Error; err != nil {
		_ = db.Exec("DELETE FROM wine_ticket_lots WHERE id = ?", lotID).Error
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM product_stocks WHERE id = ?", stockID).Error
		_ = db.Exec("DELETE FROM wine_ticket_lots WHERE id = ?", lotID).Error
	})

	repo := NewRepository(db)
	service := &Service{repo: repo}
	handler := &mysqlLotReceiveHandler{
		stubReturnSettlementHandler: stubReturnSettlementHandler{
			key: ReturnSettlementKey{
				OrderType:      "wine_ticket_redemption",
				SettlementMode: "wine_ticket",
			},
			settlementType: SettlementWineTicketRestore,
		},
		lotID:     lotID,
		attempted: make(chan struct{}),
	}

	createTx := db.WithContext(t.Context()).Begin()
	if createTx.Error != nil {
		t.Fatal(createTx.Error)
	}
	defer func() { _ = createTx.Rollback().Error }()
	if err := createTx.Exec("SET innodb_lock_wait_timeout = 5").Error; err != nil {
		t.Fatal(err)
	}
	var lotLock struct{ ID uint64 }
	if err := createTx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Table("wine_ticket_lots").
		Select("id").
		Where("id = ?", lotID).
		Take(&lotLock).Error; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	returnResult := make(chan error, 1)
	go func() {
		returnResult <- db.WithContext(ctx).Transaction(func(returnTx *gorm.DB) error {
			if err := returnTx.Exec("SET innodb_lock_wait_timeout = 5").Error; err != nil {
				return err
			}
			_, stocks, prepareErr := service.prepareReceivedSettlementAndLockStocks(
				ctx,
				returnTx,
				Return{},
				AfterSale{},
				OrderRef{},
				handler,
				[]uint64{shopProductID},
			)
			if prepareErr != nil {
				return prepareErr
			}
			if len(stocks) != 1 {
				return gorm.ErrRecordNotFound
			}
			return nil
		})
	}()

	select {
	case <-handler.attempted:
	case err := <-returnResult:
		t.Fatalf("return transaction ended before lot pre-lock: %v", err)
	case <-ctx.Done():
		t.Fatal("return transaction never attempted the lot pre-lock")
	}
	if _, err := repo.LockStocks(ctx, createTx, []uint64{shopProductID}); err != nil {
		t.Fatalf("create transaction lock stock: %v", err)
	}
	if err := createTx.Commit().Error; err != nil {
		t.Fatalf("commit create transaction: %v", err)
	}
	select {
	case err := <-returnResult:
		if err != nil {
			t.Fatalf("prepared return deadlocked with lots-to-stock transaction: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("prepared return did not finish after the lots-to-stock transaction committed")
	}
}

func TestReturnSettlementRegistryRejectsDuplicateTypeAndRoute(t *testing.T) {
	t.Parallel()
	registry := newReturnSettlementRegistry()
	first := stubReturnSettlementHandler{
		key: ReturnSettlementKey{
			OrderType:      "wine_ticket_redemption",
			SettlementMode: "wine_ticket",
		},
		settlementType: SettlementWineTicketRestore,
	}
	if err := registry.register(first); err != nil {
		t.Fatalf("register first handler: %v", err)
	}
	if err := registry.register(first); err == nil {
		t.Fatal("expected duplicate route registration to fail")
	}
	sameType := stubReturnSettlementHandler{
		key: ReturnSettlementKey{
			OrderType:      "another_order",
			SettlementMode: "another_mode",
		},
		settlementType: SettlementWineTicketRestore,
	}
	if err := registry.register(sameType); err == nil {
		t.Fatal("expected duplicate settlement type registration to fail")
	}
}

func TestWineTicketReturnBindingRequiresBusinessID(t *testing.T) {
	t.Parallel()
	handler := stubReturnSettlementHandler{
		key: ReturnSettlementKey{
			OrderType:      "wine_ticket_redemption",
			SettlementMode: "wine_ticket",
		},
		settlementType: SettlementWineTicketRestore,
	}
	err := validateReturnSettlementBinding(handler, ReturnSettlementBinding{
		SettlementType: SettlementWineTicketRestore,
	})
	if err == nil {
		t.Fatal("expected missing wine-ticket business id to fail closed")
	}
}
