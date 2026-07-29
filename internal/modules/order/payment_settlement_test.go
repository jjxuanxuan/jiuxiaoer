package order

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type paymentSettlementTestBusiness struct {
	ID     uint64 `gorm:"primaryKey;autoIncrement:false"`
	Status string
}

func (paymentSettlementTestBusiness) TableName() string {
	return "payment_settlement_test_businesses"
}

type paymentSettlementTestHandler struct {
	businessType        string
	events              []string
	fact                PaymentSettlementFact
	successFailures     int
	persistFailureState bool
}

func (h *paymentSettlementTestHandler) BusinessType() string {
	if h.businessType != "" {
		return h.businessType
	}
	return "wine_ticket_purchase"
}

func (h *paymentSettlementTestHandler) LockBusiness(ctx context.Context, tx *gorm.DB, id uint64) error {
	h.events = append(h.events, "lock")
	return tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&paymentSettlementTestBusiness{}, "id = ?", id).Error
}

func (h *paymentSettlementTestHandler) ApplySuccess(ctx context.Context, tx *gorm.DB, fact PaymentSettlementFact) error {
	h.events = append(h.events, "success")
	h.fact = fact
	if h.successFailures > 0 {
		h.successFailures--
		return errors.New("injected business settlement failure")
	}
	return tx.WithContext(ctx).Model(&paymentSettlementTestBusiness{}).
		Where("id = ? AND status IN ?", fact.BizID, []string{"pending_payment", "settlement_exception"}).
		Update("status", "paid").Error
}

func (h *paymentSettlementTestHandler) ApplyTerminal(context.Context, *gorm.DB, PaymentSettlementFact) error {
	h.events = append(h.events, "terminal")
	return nil
}

func (h *paymentSettlementTestHandler) ApplyException(
	ctx context.Context,
	tx *gorm.DB,
	fact PaymentSettlementFact,
	_ string,
) error {
	h.events = append(h.events, "exception")
	h.fact = fact
	if h.persistFailureState {
		return tx.WithContext(ctx).Model(&paymentSettlementTestBusiness{}).
			Where("id = ?", fact.BizID).
			Update("status", "settlement_exception").Error
	}
	return nil
}

func TestExternalPaymentSettlementUsesRegisteredBusinessHandler(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Payment{}, &paymentSettlementTestBusiness{}); err != nil {
		t.Fatal(err)
	}
	// 生产支付表包含基础迁移提供的软删除字段。
	// Payment 有意不向业务代码暴露该字段。
	if err := db.Exec("ALTER TABLE payments ADD COLUMN deleted_at datetime").Error; err != nil {
		t.Fatal(err)
	}

	bizType := "wine_ticket_purchase"
	bizID := uint64(701)
	if err := db.Create(&paymentSettlementTestBusiness{ID: bizID, Status: "pending_payment"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Payment{
		ID: 801, PaymentNo: "WT-PAY-801", BizType: &bizType, BizID: &bizID,
		CustomerID: 91, Channel: "wechat_miniapp", Provider: "wechat",
		Status: "pending", Amount: 23800, Currency: "CNY",
	}).Error; err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{}
	cfg.WeChat.PayMockEnabled = true
	handler := &paymentSettlementTestHandler{}
	service := NewService(cfg, db, snowflake.New(91)).WithPaymentSettlementHandler(handler)
	paidAt := time.Now().Truncate(time.Second)
	result, err := service.ApplyProviderPaymentState(context.Background(), "WT-PAY-801", "wechat", ProviderPaymentState{
		PaymentNo: "WT-PAY-801", ProviderTradeNo: "WX-801", Status: "SUCCESS",
		Amount: 23800, Currency: "CNY", AmountPresent: true, PaidAt: &paidAt,
	}, "system", 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" {
		t.Fatalf("payment status=%q", result.Status)
	}
	if strings.Join(handler.events, ",") != "lock,success" {
		t.Fatalf("handler events=%v", handler.events)
	}
	if handler.fact.BizType != bizType || handler.fact.BizID != bizID || handler.fact.Amount != 23800 {
		t.Fatalf("unexpected settlement fact: %+v", handler.fact)
	}
	var business paymentSettlementTestBusiness
	if err := db.First(&business, "id = ?", bizID).Error; err != nil {
		t.Fatal(err)
	}
	if business.Status != "paid" {
		t.Fatalf("business status=%q", business.Status)
	}
}

func TestWineTicketRenewalSuccessWithoutPaidAtBecomesException(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(
		sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Payment{}, &paymentSettlementTestBusiness{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE payments ADD COLUMN deleted_at datetime").Error; err != nil {
		t.Fatal(err)
	}

	bizType := wineTicketRenewalPaymentBusiness
	bizID := uint64(702)
	if err := db.Create(&paymentSettlementTestBusiness{
		ID:     bizID,
		Status: "pending_payment",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Payment{
		ID:         802,
		PaymentNo:  "WT-RENEW-PAY-802",
		BizType:    &bizType,
		BizID:      &bizID,
		CustomerID: 91,
		Channel:    "wechat_miniapp",
		Provider:   "wechat",
		Status:     "pending",
		Amount:     23800,
		Currency:   "CNY",
	}).Error; err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{}
	cfg.WeChat.PayMockEnabled = true
	handler := &paymentSettlementTestHandler{businessType: bizType}
	service := NewService(cfg, db, snowflake.New(92)).
		WithPaymentSettlementHandler(handler)

	_, err = service.ApplyProviderPaymentState(
		context.Background(),
		"WT-RENEW-PAY-802",
		"wechat",
		ProviderPaymentState{
			PaymentNo:       "WT-RENEW-PAY-802",
			ProviderTradeNo: "WX-802",
			Status:          "SUCCESS",
			Amount:          23800,
			Currency:        "CNY",
			AmountPresent:   true,
		},
		"system",
		0,
		"test:missing-paid-at",
	)
	if problem.FromError(err).ErrorCode != "PAYMENT_PROVIDER_DATA_MISMATCH" {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(handler.events, ",") != "lock,exception" {
		t.Fatalf("handler events=%v", handler.events)
	}
	if handler.fact.PaidAt != nil {
		t.Fatalf("missing provider paid_at must remain nil: %+v", handler.fact)
	}

	var payment Payment
	if err := db.First(&payment, "id = ?", 802).Error; err != nil {
		t.Fatal(err)
	}
	if payment.Status != "exception" ||
		payment.FailureCode == nil ||
		*payment.FailureCode != "PROVIDER_PAID_AT_MISSING" ||
		payment.PaidAt != nil {
		t.Fatalf("unexpected payment: %+v", payment)
	}
	var business paymentSettlementTestBusiness
	if err := db.First(&business, "id = ?", bizID).Error; err != nil {
		t.Fatal(err)
	}
	if business.Status != "pending_payment" {
		t.Fatalf("business status=%q", business.Status)
	}
}

func TestExternalSuccessSettlementFailurePersistsAndReplays(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(
		sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&Payment{},
		&paymentSettlementTestBusiness{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		"ALTER TABLE payments ADD COLUMN deleted_at datetime",
	).Error; err != nil {
		t.Fatal(err)
	}

	bizType := "wine_ticket_purchase"
	bizID := uint64(703)
	if err := db.Create(&paymentSettlementTestBusiness{
		ID: bizID, Status: "pending_payment",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Payment{
		ID: 803, PaymentNo: "WT-PAY-803", BizType: &bizType, BizID: &bizID,
		CustomerID: 91, Channel: "wechat_miniapp", Provider: "wechat",
		Status: "pending", Amount: 23800, Currency: "CNY",
	}).Error; err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.WeChat.PayMockEnabled = true
	handler := &paymentSettlementTestHandler{
		successFailures:     1,
		persistFailureState: true,
	}
	service := NewService(cfg, db, snowflake.New(93)).
		WithPaymentSettlementHandler(handler)
	paidAt := time.Now().Truncate(time.Millisecond)
	state := ProviderPaymentState{
		PaymentNo: "WT-PAY-803", ProviderTradeNo: "WX-803",
		Status: "SUCCESS", Amount: 23800, Currency: "CNY",
		AmountPresent: true, PaidAt: &paidAt,
	}

	if _, err := service.ApplyProviderPaymentState(
		context.Background(),
		"WT-PAY-803",
		"wechat",
		state,
		"system",
		0,
		"first-attempt",
	); err == nil {
		t.Fatal("injected settlement failure was not returned")
	}
	var payment Payment
	if err := db.First(&payment, 803).Error; err != nil {
		t.Fatal(err)
	}
	if payment.Status != "exception" ||
		payment.ProviderStatus == nil ||
		*payment.ProviderStatus != "SUCCESS" ||
		payment.ProviderTradeNo == nil ||
		*payment.ProviderTradeNo != "WX-803" ||
		payment.PaidAt == nil ||
		payment.NextReconcileAt == nil {
		t.Fatalf("verified failure fact was not persisted: %+v", payment)
	}
	var business paymentSettlementTestBusiness
	if err := db.First(&business, bizID).Error; err != nil {
		t.Fatal(err)
	}
	if business.Status != "settlement_exception" {
		t.Fatalf("business failure status=%q", business.Status)
	}

	result, err := service.ApplyProviderPaymentState(
		context.Background(),
		"WT-PAY-803",
		"wechat",
		state,
		"system",
		0,
		"replay",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" {
		t.Fatalf("replayed payment=%+v", result)
	}
	if err := db.First(&business, bizID).Error; err != nil {
		t.Fatal(err)
	}
	if business.Status != "paid" {
		t.Fatalf("replayed business status=%q", business.Status)
	}
}

func TestPaymentSettlementRegistryRejectsUnsafeRegistrations(t *testing.T) {
	service := &Service{}
	handler := &paymentSettlementTestHandler{}
	service.WithPaymentSettlementHandler(handler)

	defer func() {
		if recover() == nil {
			t.Fatal("duplicate settlement registration must panic")
		}
	}()
	service.WithPaymentSettlementHandler(handler)
}

type paymentSettlementCallbackProvider struct {
	event PaymentCallbackEvent
}

func (p *paymentSettlementCallbackProvider) Code() string {
	return "wechat"
}

func (p *paymentSettlementCallbackProvider) Create(context.Context, CreateProviderPaymentInput) (ProviderPaymentResult, error) {
	return ProviderPaymentResult{}, nil
}

func (p *paymentSettlementCallbackProvider) Query(context.Context, string) (ProviderPaymentState, error) {
	return ProviderPaymentState{}, nil
}

func (p *paymentSettlementCallbackProvider) Close(context.Context, string) (ProviderOperationResult, error) {
	return ProviderOperationResult{}, nil
}

func (p *paymentSettlementCallbackProvider) ParseCallback(context.Context, *http.Request) (PaymentCallbackEvent, error) {
	return p.event, nil
}

func (p *paymentSettlementCallbackProvider) Shutdown() {}

func TestExternalPaymentCallbackWithoutHandlerStaysFailed(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Payment{}, &PaymentCallback{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE payments ADD COLUMN deleted_at datetime").Error; err != nil {
		t.Fatal(err)
	}

	bizType := "wine_ticket_purchase"
	bizID := uint64(1701)
	if err := db.Create(&Payment{
		ID: 1801, PaymentNo: "WT-PAY-1801", BizType: &bizType, BizID: &bizID,
		CustomerID: 92, Channel: "wechat_miniapp", Provider: "wechat",
		Status: "pending", Amount: 19800, Currency: "CNY",
	}).Error; err != nil {
		t.Fatal(err)
	}

	paidAt := time.Now()
	provider := &paymentSettlementCallbackProvider{event: PaymentCallbackEvent{
		EventID: "WT-CALLBACK-1801", PaymentNo: "WT-PAY-1801", ProviderTradeNo: "WX-1801",
		Status: "SUCCESS", Amount: 19800, Currency: "CNY", PaidAt: &paidAt,
	}}
	cfg := config.Config{}
	cfg.WeChat.PayMockEnabled = true
	service := NewService(cfg, db, snowflake.New(92)).
		WithPaymentProvider(provider, metrics.New("payment-settlement-test", ""))

	request, err := http.NewRequest(http.MethodPost, "/callbacks", bytes.NewReader([]byte(`{"event":"1801"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessPaymentCallback(context.Background(), "wechat", request, []byte(`{"event":"1801"}`)); err == nil {
		t.Fatal("missing external settlement handler must reject callback")
	}

	var callback PaymentCallback
	if err := db.Where("provider = ? AND provider_event_id = ?", "wechat", "WT-CALLBACK-1801").First(&callback).Error; err != nil {
		t.Fatal(err)
	}
	if callback.ProcessStatus != "failed" || callback.ErrorCode == nil || *callback.ErrorCode != "PAYMENT_SETTLEMENT_HANDLER_NOT_FOUND" {
		t.Fatalf("unexpected callback result: %+v", callback)
	}
}

func TestExternalPaymentCallbackSettlementFailureIsDurableAndLaterAcked(
	t *testing.T,
) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(
		sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&Payment{},
		&PaymentCallback{},
		&paymentSettlementTestBusiness{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		"ALTER TABLE payments ADD COLUMN deleted_at datetime",
	).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		CREATE UNIQUE INDEX uk_payment_settlement_callback_event
		ON payment_callbacks(provider, provider_event_id)
	`).Error; err != nil {
		t.Fatal(err)
	}
	bizType := "wine_ticket_purchase"
	bizID := uint64(1702)
	if err := db.Create(&paymentSettlementTestBusiness{
		ID: bizID, Status: "pending_payment",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Payment{
		ID: 1802, PaymentNo: "WT-PAY-1802", BizType: &bizType, BizID: &bizID,
		CustomerID: 92, Channel: "wechat_miniapp", Provider: "wechat",
		Status: "pending", Amount: 19800, Currency: "CNY",
	}).Error; err != nil {
		t.Fatal(err)
	}
	paidAt := time.Now().Truncate(time.Millisecond)
	event := PaymentCallbackEvent{
		EventID: "WT-CALLBACK-1802", PaymentNo: "WT-PAY-1802",
		ProviderTradeNo: "WX-1802", Status: "SUCCESS",
		Amount: 19800, Currency: "CNY", PaidAt: &paidAt,
	}
	provider := &paymentSettlementCallbackProvider{event: event}
	cfg := config.Config{}
	cfg.WeChat.PayMockEnabled = true
	handler := &paymentSettlementTestHandler{
		successFailures:     1,
		persistFailureState: true,
	}
	service := NewService(cfg, db, snowflake.New(94)).
		WithPaymentProvider(
			provider,
			metrics.New("payment-settlement-test", ""),
		).
		WithPaymentSettlementHandler(handler)
	body := []byte(`{"event":"1802"}`)

	request, err := http.NewRequest(
		http.MethodPost,
		"/callbacks",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessPaymentCallback(
		context.Background(),
		"wechat",
		request,
		body,
	); err == nil {
		t.Fatal("injected callback settlement failure was not returned")
	}
	var callback PaymentCallback
	if err := db.Where(
		"provider = ? AND provider_event_id = ?",
		"wechat",
		event.EventID,
	).First(&callback).Error; err != nil {
		t.Fatal(err)
	}
	if callback.ProcessStatus != "failed" ||
		callback.PaymentID == nil ||
		*callback.PaymentID != 1802 {
		t.Fatalf("failed callback fact=%+v", callback)
	}
	var payment Payment
	if err := db.First(&payment, 1802).Error; err != nil {
		t.Fatal(err)
	}
	if payment.Status != "exception" ||
		payment.ProviderStatus == nil ||
		*payment.ProviderStatus != "SUCCESS" {
		t.Fatalf("payment failure fact=%+v", payment)
	}

	state := ProviderPaymentState{
		PaymentNo: event.PaymentNo, ProviderTradeNo: event.ProviderTradeNo,
		Status: event.Status, Amount: event.Amount, Currency: event.Currency,
		AmountPresent: true, PaidAt: event.PaidAt,
	}
	if _, err := service.ApplyProviderPaymentState(
		context.Background(),
		event.PaymentNo,
		"wechat",
		state,
		"system",
		0,
		"worker-replay",
	); err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest(
		http.MethodPost,
		"/callbacks",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessPaymentCallback(
		context.Background(),
		"wechat",
		request,
		body,
	); err != nil {
		t.Fatalf("converged duplicate callback was not acknowledged: %v", err)
	}
	var convergedCallback PaymentCallback
	if err := db.First(&convergedCallback, callback.ID).Error; err != nil {
		t.Fatal(err)
	}
	if convergedCallback.ProcessStatus != "processed" ||
		convergedCallback.ErrorCode != nil {
		t.Fatalf("converged callback=%+v", convergedCallback)
	}
}
