package renewal

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const renewalTestQuoteSecret = "renewal-test-quote-secret-32-bytes-minimum"

type renewalTestOutbox struct {
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

func (renewalTestOutbox) TableName() string { return "outbox_events" }

type renewalTestRealname struct {
	CustomerID  uint64 `gorm:"primaryKey"`
	Status      string
	AdultResult string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

func (renewalTestRealname) TableName() string {
	return "customer_realname_verifications"
}

type renewalTestVerification struct {
	ID               uint64 `gorm:"primaryKey;autoIncrement:false"`
	CustomerID       uint64
	Status           string
	SessionExpiresAt *time.Time
}

func (renewalTestVerification) TableName() string {
	return "identity_verification_requests"
}

type renewalPaymentProvider struct {
	mu    sync.Mutex
	state order.ProviderPaymentState
}

func (p *renewalPaymentProvider) Code() string { return "wechat" }

func (p *renewalPaymentProvider) Create(
	_ context.Context,
	input order.CreateProviderPaymentInput,
) (order.ProviderPaymentResult, error) {
	return order.ProviderPaymentResult{
		Status:           "NOTPAY",
		ProviderPrepayID: "prepay-" + input.PaymentNo,
		RequestID:        "renewal-create-request",
		ClientPayload: map[string]any{
			"timeStamp": "1785000000",
			"nonceStr":  "renewal-nonce",
			"package":   "prepay_id=prepay-" + input.PaymentNo,
			"signType":  "RSA",
			"paySign":   "renewal-signature",
		},
	}, nil
}

func (p *renewalPaymentProvider) Query(
	_ context.Context,
	paymentNo string,
) (order.ProviderPaymentState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.state
	state.PaymentNo = paymentNo
	return state, nil
}

func (p *renewalPaymentProvider) Close(
	context.Context,
	string,
) (order.ProviderOperationResult, error) {
	return order.ProviderOperationResult{RequestID: "renewal-close-request"}, nil
}

func (p *renewalPaymentProvider) ParseCallback(
	context.Context,
	*http.Request,
) (order.PaymentCallbackEvent, error) {
	return order.PaymentCallbackEvent{}, nil
}

func (p *renewalPaymentProvider) Shutdown() {}

func (p *renewalPaymentProvider) setState(state order.ProviderPaymentState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
}

func newRenewalTestDB(t *testing.T) *gorm.DB {
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
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&purchasedomain.Purchase{},
		&core.Lot{},
		&core.Transaction{},
		&redemption.RedemptionAllocation{},
		&gift.GiftAllocation{},
		&refunddomain.RefundAllocation{},
		&Renewal{},
		&order.Payment{},
		&refund.Row{},
		&idempotency.Record{},
		&auth.Customer{},
		&auth.CustomerIdentity{},
		&renewalTestRealname{},
		&renewalTestVerification{},
		&order.AuditLog{},
		&renewalTestOutbox{},
	); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"ALTER TABLE customers ADD COLUMN deleted_at datetime",
		"ALTER TABLE customer_identities ADD COLUMN deleted_at datetime",
		"ALTER TABLE payments ADD COLUMN deleted_at datetime",
		"ALTER TABLE refunds ADD COLUMN deleted_at datetime",
		"CREATE UNIQUE INDEX uk_renewal_test_idempotency ON idempotency_keys(actor_type, actor_id, path, key_hash)",
		"CREATE UNIQUE INDEX uk_renewal_test_no ON wine_ticket_renewals(renewal_no)",
		"CREATE UNIQUE INDEX uk_renewal_test_payment ON wine_ticket_renewals(payment_id)",
		"CREATE UNIQUE INDEX uk_renewal_test_active_lot ON wine_ticket_renewals(lot_id) WHERE status IN ('pending_payment','payment_unknown','applying','compensating_refund','refund_exception')",
		"CREATE UNIQUE INDEX uk_renewal_test_transaction_action ON wine_ticket_transactions(action_key)",
		"CREATE UNIQUE INDEX uk_renewal_test_refund_no ON refunds(refund_no)",
		"CREATE UNIQUE INDEX uk_renewal_test_refund_replaces ON refunds(replaces_refund_id)",
		"CREATE UNIQUE INDEX uk_renewal_test_outbox_event ON outbox_events(event_id)",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func renewalCustomerClaims(
	customerID uint64,
	permissions ...string,
) *auth.Claims {
	return &auth.Claims{
		AccountType: "customer",
		CustomerID:  idString(customerID),
		Permissions: permissions,
	}
}

func seedRenewalLot(
	t *testing.T,
	db *gorm.DB,
	now time.Time,
	customerID, purchaseID, lotID uint64,
	fee int64,
	maxCount int,
) (purchasedomain.Purchase, core.Lot) {
	t.Helper()
	policy := datatypes.JSON(
		`{"schema_version":1,"enabled":true,"extension_days":30,"max_count":` +
			strconv.Itoa(maxCount) +
			`,"grace_days":0,"fee_amount":` +
			strconv.FormatInt(fee, 10) +
			`}`,
	)
	purchase := purchasedomain.Purchase{
		ID:                       purchaseID,
		PurchaseNo:               "WTPU" + idString(purchaseID),
		CustomerID:               customerID,
		PackageID:                purchaseID + 100,
		PaymentID:                purchaseID + 200,
		ProductID:                401,
		IssuerMerchantID:         1,
		RedeemCityCode:           "310100",
		PackageQuantity:          1,
		BottleQuantityPerPackage: 6,
		TotalBottleQuantity:      6,
		UnitPriceAmount:          1000,
		PayableAmount:            6000,
		Currency:                 "CNY",
		PackageSnapshot:          datatypes.JSON(`{"schema_version":1}`),
		RefundPolicySnapshot:     datatypes.JSON(`{"schema_version":1,"enabled":false,"window_hours":0,"require_never_used":true,"fee_amount":0}`),
		RenewalPolicySnapshot:    policy,
		Status:                   purchasedomain.PurchaseStatusIssued,
		Version:                  1,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	expiry := now.AddDate(0, 0, 5)
	lot := core.Lot{
		ID:                lotID,
		LotNo:             "WTL" + idString(lotID),
		OwnerCustomerID:   customerID,
		PurchaseID:        purchase.ID,
		SourceType:        LotSourcePurchase,
		IssuerMerchantID:  1,
		ProductID:         401,
		RedeemCityCode:    "310100",
		TotalQuantity:     6,
		AvailableQuantity: 6,
		OriginalExpiresAt: expiry,
		ExpiresAt:         expiry,
		ExpiryChangedAt:   now,
		Status:            LotStatusActive,
		Version:           1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := db.Create(&purchase).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	return purchase, lot
}

func TestRenewalQuoteAndFreeCreateAreSignedAtomicAndIdempotent(t *testing.T) {
	db := newRenewalTestDB(t)
	now := time.Date(
		2026,
		7,
		27,
		9,
		15,
		0,
		123000000,
		shanghaiLocation,
	)
	service := NewRenewalService(
		db,
		snowflake.New(301),
		renewalTestQuoteSecret,
	).WithRenewalClock(func() time.Time { return now })
	_, originalLot := seedRenewalLot(t, db, now, 9001, 9101, 9201, 0, 2)
	claims := renewalCustomerClaims(
		9001,
		"wine_ticket_renewal:quote",
		"wine_ticket_renewal:create",
		"wine_ticket_renewal:view",
	)

	quote, err := service.RenewalQuote(
		context.Background(),
		claims,
		originalLot.LotNo,
	)
	if err != nil {
		t.Fatal(err)
	}
	expectedExpiry := originalLot.ExpiresAt.In(shanghaiLocation).
		AddDate(0, 0, 30).
		Truncate(time.Millisecond)
	if !quote.Eligible ||
		quote.ExpectedVersion != originalLot.Version ||
		quote.NewExpiresAt != formatShanghai(expectedExpiry) ||
		quote.FeeAmount != 0 ||
		!strings.HasSuffix(quote.NewExpiresAt, "+08:00") {
		t.Fatalf("unexpected renewal quote: %+v", quote)
	}

	tampered := quote.QuoteToken[:len(quote.QuoteToken)-1] + "A"
	_, err = service.CreateRenewal(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/wine-tickets/lots/:lot_no/renewals",
		"renew-free-tampered-0001",
		originalLot.LotNo,
		RenewalCreateRequest{
			ExpectedLotVersion: quote.ExpectedVersion,
			QuoteToken:         tampered,
		},
	)
	assertRenewalProblem(t, err, http.StatusConflict, "WT_RENEWAL_QUOTE_EXPIRED")

	request := RenewalCreateRequest{
		ExpectedLotVersion: quote.ExpectedVersion,
		QuoteToken:         quote.QuoteToken,
	}
	first, err := service.CreateRenewal(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/wine-tickets/lots/:lot_no/renewals",
		"renew-free-create-0001",
		originalLot.LotNo,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	quoteExpiredAt, err := time.Parse(time.RFC3339Nano, quote.QuoteExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	now = quoteExpiredAt.Add(time.Millisecond)
	replay, err := service.CreateRenewal(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/wine-tickets/lots/:lot_no/renewals",
		"renew-free-create-0001",
		originalLot.LotNo,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.RenewalNo != replay.RenewalNo ||
		first.Status != RenewalStatusCompleted ||
		first.NewExpiresAt != formatShanghai(expectedExpiry) {
		t.Fatalf("first=%+v replay=%+v", first, replay)
	}

	var lot core.Lot
	if err := db.First(&lot, "id = ?", originalLot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !lot.ExpiresAt.Equal(expectedExpiry) ||
		lot.RenewalCount != 1 ||
		!lot.EverUsed ||
		lot.Version != 2 {
		t.Fatalf("unexpected renewed lot: %+v", lot)
	}
	var renewalCount, transactionCount, renewedEventCount int64
	db.Model(&Renewal{}).Count(&renewalCount)
	db.Model(&core.Transaction{}).Count(&transactionCount)
	db.Model(&renewalTestOutbox{}).
		Where("event_type = ?", "wine_ticket.renewed").
		Count(&renewedEventCount)
	if renewalCount != 1 || transactionCount != 0 || renewedEventCount != 1 {
		t.Fatalf(
			"renewals=%d transactions=%d renewed_events=%d",
			renewalCount,
			transactionCount,
			renewedEventCount,
		)
	}

	items, _, err := service.ListRenewals(
		context.Background(),
		claims,
		pagination.Query{PageSize: 20},
		RenewalStatusCompleted,
	)
	if err != nil || len(items) != 1 || items[0].RenewalNo != first.RenewalNo {
		t.Fatalf("list items=%+v err=%v", items, err)
	}
}

func TestPaidRenewalUsesCommonPaymentAndAppliesFixedTargetOnce(t *testing.T) {
	db := newRenewalTestDB(t)
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	ids := snowflake.New(302)
	provider := &renewalPaymentProvider{}
	cfg := config.Config{}
	cfg.WeChat.PayEnabled = true
	cfg.WeChat.PayMockEnabled = true
	cfg.WeChat.HTTPTimeout = time.Second
	paymentService := order.NewService(cfg, db, ids).
		WithPaymentProvider(provider, metrics.New("renewal-payment-test", ""))
	service := NewRenewalService(
		db,
		ids,
		renewalTestQuoteSecret,
	).
		WithPaymentService(paymentService).
		WithWeChatAppID("wx-renewal-test").
		WithRenewalClock(func() time.Time { return now })
	paymentService.WithPaymentSettlementHandler(
		NewRenewalPaymentSettlementHandler(service),
	)
	_, originalLot := seedRenewalLot(t, db, now, 9301, 9401, 9501, 880, 2)
	if err := db.Create(&auth.Customer{
		ID:     9301,
		Status: "active",
		Phone:  "13800000000",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&auth.CustomerIdentity{
		ID:              9601,
		CustomerID:      9301,
		Provider:        "wechat_miniapp",
		AppID:           "wx-renewal-test",
		ProviderSubject: "openid-renewal-test",
		Status:          "active",
	}).Error; err != nil {
		t.Fatal(err)
	}
	claims := renewalCustomerClaims(
		9301,
		"wine_ticket_renewal:quote",
		"wine_ticket_renewal:create",
		"wine_ticket_renewal:view",
		"wine_ticket_payment:confirm",
	)

	quote, err := service.RenewalQuote(
		context.Background(),
		claims,
		originalLot.LotNo,
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateRenewal(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/wine-tickets/lots/:lot_no/renewals",
		"renew-paid-create-0001",
		originalLot.LotNo,
		RenewalCreateRequest{
			ExpectedLotVersion: quote.ExpectedVersion,
			QuoteToken:         quote.QuoteToken,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != RenewalStatusPendingPayment ||
		created.FeeAmount != 880 ||
		created.PaymentParameters == nil {
		t.Fatalf("unexpected paid renewal draft: %+v", created)
	}
	var guardedLot core.Lot
	if err := db.First(&guardedLot, "id = ?", originalLot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !guardedLot.ExpiresAt.Equal(originalLot.ExpiresAt) ||
		guardedLot.RenewalCount != 0 ||
		!guardedLot.EverUsed ||
		guardedLot.Version != 2 {
		t.Fatalf("paid guard changed lot facts incorrectly: %+v", guardedLot)
	}

	paidAt := originalLot.ExpiresAt.Add(-time.Minute)
	provider.setState(order.ProviderPaymentState{
		Status:          "SUCCESS",
		ProviderTradeNo: "WX-RENEWAL-PAID",
		Amount:          880,
		Currency:        "CNY",
		AmountPresent:   true,
		PaidAt:          &paidAt,
		RequestID:       "renewal-query-success",
	})
	confirmed, err := service.ConfirmRenewalPayment(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/wine-tickets/renewals/:renewal_no/payment/confirm",
		"renew-paid-confirm-0001",
		created.RenewalNo,
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.ConfirmRenewalPayment(
		context.Background(),
		claims,
		http.MethodPost,
		"/api/v1/wine-tickets/renewals/:renewal_no/payment/confirm",
		"renew-paid-confirm-0001",
		created.RenewalNo,
	)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != RenewalStatusCompleted ||
		replay.Status != RenewalStatusCompleted ||
		confirmed.RenewalNo != replay.RenewalNo {
		t.Fatalf("confirmed=%+v replay=%+v", confirmed, replay)
	}
	var renewedLot core.Lot
	if err := db.First(&renewedLot, "id = ?", originalLot.ID).Error; err != nil {
		t.Fatal(err)
	}
	expectedExpiry := originalLot.ExpiresAt.In(shanghaiLocation).
		AddDate(0, 0, 30).
		Truncate(time.Millisecond)
	if renewedLot.RenewalCount != 1 ||
		!renewedLot.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf("paid renewal did not apply fixed target: %+v", renewedLot)
	}
	var transactionCount int64
	db.Model(&core.Transaction{}).Count(&transactionCount)
	if transactionCount != 0 {
		t.Fatalf("renewal must not create zero quantity transactions: %d", transactionCount)
	}
}

func TestPaidRenewalFailedDraftRecoveryClosesAndExpiresGuard(t *testing.T) {
	db := newRenewalTestDB(t)
	seedNow := time.Date(
		2026,
		7,
		27,
		14,
		0,
		0,
		123000000,
		shanghaiLocation,
	)
	ids := snowflake.New(308)
	provider := &renewalPaymentProvider{}
	paymentService := order.NewService(config.Config{}, db, ids).
		WithPaymentProvider(
			provider,
			metrics.New("renewal-failed-draft-recovery-test", ""),
		)
	serviceNow := seedNow
	service := NewRenewalService(
		db,
		ids,
		renewalTestQuoteSecret,
	).
		WithPaymentService(paymentService).
		WithRenewalClock(func() time.Time { return serviceNow })
	lot, renewal, payment := seedPendingRenewal(
		t,
		db,
		seedNow,
		10_101,
		10_111,
		10_121,
		10_131,
		10_141,
		880,
	)

	const (
		path = "/api/v1/wine-tickets/lots/:lot_no/renewals"
		key  = "renew-failed-draft-recovery-0001"
	)
	req := RenewalCreateRequest{
		ExpectedLotVersion: renewal.ExpectedLotVersion,
		QuoteToken:         strings.Repeat("x", 32),
	}
	serviceNow = lot.ExpiresAt
	failedAt := serviceNow.Add(-time.Millisecond)
	if err := db.Model(&order.Payment{}).
		Where("id = ?", payment.ID).
		Updates(map[string]any{
			"status":          "failed",
			"failure_code":    "PROVIDER_REJECTED",
			"failed_at":       failedAt,
			"idempotency_key": key,
		}).Error; err != nil {
		t.Fatal(err)
	}
	lockedUntil := serviceNow.Add(-time.Second)
	if err := db.Create(&idempotency.Record{
		ID:        ids.Next(),
		ActorType: "customer",
		ActorID:   renewal.CustomerID,
		Method:    http.MethodPost,
		Path:      path,
		KeyHash:   idempotency.KeyHash(key),
		RequestHash: idempotency.ResourceRequestHash(
			"wine_ticket.renewal.create",
			lot.LotNo,
			req,
		),
		Status:      "processing",
		LockedUntil: &lockedUntil,
		ExpiredAt:   serviceNow.Add(24 * time.Hour),
		CreatedAt:   seedNow,
		UpdatedAt:   seedNow,
	}).Error; err != nil {
		t.Fatal(err)
	}
	claims := renewalCustomerClaims(
		renewal.CustomerID,
		"wine_ticket_renewal:create",
	)

	recovered, err := service.CreateRenewal(
		context.Background(),
		claims,
		http.MethodPost,
		path,
		key,
		lot.LotNo,
		req,
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.CreateRenewal(
		context.Background(),
		claims,
		http.MethodPost,
		path,
		key,
		lot.LotNo,
		req,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != RenewalStatusClosed ||
		replayed.Status != RenewalStatusClosed ||
		recovered.RenewalNo != renewal.RenewalNo ||
		replayed.RenewalNo != renewal.RenewalNo {
		t.Fatalf("recovered=%+v replayed=%+v", recovered, replayed)
	}

	var storedRenewal Renewal
	var storedLot core.Lot
	if err := db.First(&storedRenewal, "id = ?", renewal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&storedLot, "id = ?", lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedRenewal.Status != RenewalStatusClosed ||
		storedRenewal.ClosedAt == nil ||
		storedLot.Status != LotStatusExpired ||
		storedLot.AvailableQuantity != 0 ||
		storedLot.RenewalCount != 0 {
		t.Fatalf("renewal=%+v lot=%+v", storedRenewal, storedLot)
	}
	var transactions []core.Transaction
	if err := db.Find(&transactions).Error; err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 1 ||
		transactions[0].TransactionType != transactionTypeLotExpiry ||
		transactions[0].QuantityDelta != -int(lot.AvailableQuantity) ||
		transactions[0].QuantityDelta == 0 {
		t.Fatalf("unexpected recovered expiry ledger: %+v", transactions)
	}
	if _, err := service.repo.activeRenewal(
		context.Background(),
		nil,
		lot.ID,
		false,
	); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("failed draft guard was not released: %v", err)
	}
	var renewalCount, paymentCount int64
	if err := db.Model(&Renewal{}).
		Where("customer_id = ?", renewal.CustomerID).
		Count(&renewalCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&order.Payment{}).
		Where(
			"customer_id = ? AND biz_type = ?",
			renewal.CustomerID,
			RenewalPaymentBusiness,
		).
		Count(&paymentCount).Error; err != nil {
		t.Fatal(err)
	}
	if renewalCount != 1 || paymentCount != 1 {
		t.Fatalf(
			"renewals=%d payments=%d, want one recovered draft",
			renewalCount,
			paymentCount,
		)
	}
}

func assertRenewalProblem(
	t *testing.T,
	err error,
	status int,
	code string,
) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected HTTP %d %s", status, code)
	}
	details := problem.FromError(err)
	if details.Status != status || details.ErrorCode != code {
		t.Fatalf(
			"problem=%+v, want HTTP %d %s",
			details,
			status,
			code,
		)
	}
}
