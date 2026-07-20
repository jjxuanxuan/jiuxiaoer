package wechatpay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/downloader"
	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
	"github.com/wechatpay-apiv3/wechatpay-go/core/option"
	paymentmodel "github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/jsapi"
	"github.com/wechatpay-apiv3/wechatpay-go/services/refunddomestic"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
)

const FakeCallbackSecret = "local-wechat-pay-callback-secret"

// New 创建并初始化支付提供器。
func New(ctx context.Context, cfg config.WeChatConfig) (order.PaymentProvider, error) {
	if !cfg.PayEnabled {
		return nil, nil
	}
	if cfg.PayMockEnabled {
		return &fakeProvider{appID: cfg.MiniAppID, mchID: "local-mch", secret: FakeCallbackSecret, refunds: make(map[string]refund.State)}, nil
	}
	privateKey, err := utils.LoadPrivateKeyWithPath(cfg.PayPrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load WeChat Pay private key: %w", err)
	}
	mgr := downloader.NewCertificateDownloaderMgr(ctx)
	if err := mgr.RegisterDownloaderWithPrivateKey(ctx, privateKey, cfg.PayCertSerial, cfg.PayMchID, cfg.PayAPIv3Key); err != nil {
		mgr.Stop()
		return nil, fmt.Errorf("initialize WeChat Pay platform certificates: %w", err)
	}
	client, err := core.NewClient(ctx, option.WithWechatPayAutoAuthCipherUsingDownloaderMgr(cfg.PayMchID, cfg.PayCertSerial, privateKey, mgr))
	if err != nil {
		mgr.Stop()
		return nil, fmt.Errorf("initialize WeChat Pay client: %w", err)
	}
	verifier := verifiers.NewSHA256WithRSAVerifier(mgr.GetCertificateVisitor(cfg.PayMchID))
	notifyHandler, err := notify.NewRSANotifyHandler(cfg.PayAPIv3Key, verifier)
	if err != nil {
		mgr.Stop()
		return nil, fmt.Errorf("initialize WeChat Pay callback handler: %w", err)
	}
	return &provider{
		cfg:           cfg,
		service:       &jsapi.JsapiApiService{Client: client},
		refundService: &refunddomestic.RefundsApiService{Client: client},
		notifyHandler: notifyHandler,
		downloader:    mgr,
	}, nil
}

type provider struct {
	cfg           config.WeChatConfig
	service       *jsapi.JsapiApiService
	refundService *refunddomestic.RefundsApiService
	notifyHandler *notify.Handler
	downloader    *downloader.CertificateDownloaderMgr
}

// Code 返回代码。
func (p *provider) Code() string { return "wechat" }

// Create 创建提供器支付结果。
func (p *provider) Create(ctx context.Context, input order.CreateProviderPaymentInput) (order.ProviderPaymentResult, error) {
	response, _, err := p.service.PrepayWithRequestPayment(ctx, jsapi.PrepayRequest{
		Appid:       core.String(p.cfg.MiniAppID),
		Mchid:       core.String(p.cfg.PayMchID),
		Description: core.String(input.Description),
		OutTradeNo:  core.String(input.PaymentNo),
		TimeExpire:  core.Time(input.ExpiresAt),
		NotifyUrl:   core.String(p.cfg.PayNotifyURL),
		Amount: &jsapi.Amount{
			Total:    core.Int64(input.Amount),
			Currency: core.String(input.Currency),
		},
		Payer: &jsapi.Payer{Openid: core.String(input.OpenID)},
	})
	if err != nil {
		return order.ProviderPaymentResult{}, fmt.Errorf("create WeChat Pay transaction: %w", err)
	}
	payload := map[string]any{
		"appId":     stringValue(response.Appid),
		"timeStamp": stringValue(response.TimeStamp),
		"nonceStr":  stringValue(response.NonceStr),
		"package":   stringValue(response.Package),
		"signType":  stringValue(response.SignType),
		"paySign":   stringValue(response.PaySign),
	}
	return order.ProviderPaymentResult{
		ProviderPrepayID: stringValue(response.PrepayId),
		Status:           "NOTPAY",
		ClientPayload:    payload,
	}, nil
}

// Query 查询提供器支付状态。
func (p *provider) Query(ctx context.Context, paymentNo string) (order.ProviderPaymentState, error) {
	transaction, _, err := p.service.QueryOrderByOutTradeNo(ctx, jsapi.QueryOrderByOutTradeNoRequest{
		OutTradeNo: core.String(paymentNo),
		Mchid:      core.String(p.cfg.PayMchID),
	})
	if err != nil {
		return order.ProviderPaymentState{}, fmt.Errorf("query WeChat Pay transaction: %w", err)
	}
	return transactionState(transaction), nil
}

// Close 关闭当前实例并释放相关资源。
func (p *provider) Close(ctx context.Context, paymentNo string) error {
	_, err := p.service.CloseOrder(ctx, jsapi.CloseOrderRequest{OutTradeNo: core.String(paymentNo), Mchid: core.String(p.cfg.PayMchID)})
	if err != nil {
		return fmt.Errorf("close WeChat Pay transaction: %w", err)
	}
	return nil
}

// ParseCallback 解析回调。
func (p *provider) ParseCallback(ctx context.Context, request *http.Request) (order.PaymentCallbackEvent, error) {
	transaction := new(paymentmodel.Transaction)
	notifyRequest, err := p.notifyHandler.ParseNotifyRequest(ctx, request, transaction)
	if err != nil {
		return order.PaymentCallbackEvent{}, fmt.Errorf("verify WeChat Pay callback: %w", err)
	}
	state := transactionState(transaction)
	return order.PaymentCallbackEvent{
		EventID:         notifyRequest.ID,
		ProviderTradeNo: state.ProviderTradeNo,
		PaymentNo:       state.PaymentNo,
		Status:          state.Status,
		Amount:          state.Amount,
		Currency:        state.Currency,
		PaidAt:          state.PaidAt,
		AppID:           stringValue(transaction.Appid),
		MchID:           stringValue(transaction.Mchid),
	}, nil
}

// Shutdown 平滑停止当前实例并释放相关资源。
func (p *provider) Shutdown() {
	if p != nil && p.downloader != nil {
		p.downloader.Stop()
	}
}

// Refund 返回退款。
func (p *provider) Refund(ctx context.Context, input refund.Input) (refund.State, error) {
	result, apiResult, err := p.refundService.Create(ctx, refunddomestic.CreateRequest{
		OutTradeNo: core.String(input.PaymentNo), OutRefundNo: core.String(input.RefundNo),
		Reason: core.String(input.Reason), NotifyUrl: core.String(input.NotifyURL),
		Amount: &refunddomestic.AmountReq{Refund: core.Int64(input.Amount), Total: core.Int64(input.TotalAmount), Currency: core.String(input.Currency)},
	})
	if err != nil {
		return refund.State{}, providerCallError("refund.create", apiResult, err)
	}
	if result == nil {
		return refund.State{}, providerCallError("refund.create", apiResult, errors.New("wechat refund create returned an empty response"))
	}
	state := refundState(result)
	state.RequestID = apiRequestID(apiResult)
	return state, nil
}

// QueryRefund 查询退款。
func (p *provider) QueryRefund(ctx context.Context, refundNo string) (refund.State, error) {
	result, apiResult, err := p.refundService.QueryByOutRefundNo(ctx, refunddomestic.QueryByOutRefundNoRequest{OutRefundNo: core.String(refundNo)})
	if err != nil {
		return refund.State{}, providerCallError("refund.query", apiResult, err)
	}
	if result == nil {
		return refund.State{}, providerCallError("refund.query", apiResult, errors.New("wechat refund query returned an empty response"))
	}
	state := refundState(result)
	state.RequestID = apiRequestID(apiResult)
	return state, nil
}

// ParseRefundCallback 解析退款回调。
func (p *provider) ParseRefundCallback(ctx context.Context, request *http.Request) (refund.CallbackEvent, error) {
	result := new(refundNotification)
	notifyRequest, err := p.notifyHandler.ParseNotifyRequest(ctx, request, result)
	if err != nil {
		return refund.CallbackEvent{}, fmt.Errorf("verify WeChat Pay refund callback: %w", err)
	}
	return refund.CallbackEvent{EventID: notifyRequest.ID, MchID: result.MchID, State: refundNotificationState(result)}, nil
}

type refundNotification struct {
	MchID, OutTradeNo, TransactionID, OutRefundNo, RefundID, RefundStatus string
	SuccessTime                                                           *time.Time `json:"success_time"`
	Amount                                                                struct {
		Total, Refund int64
	} `json:"amount"`
}

// UnmarshalJSON 反序列化JSON。
func (r *refundNotification) UnmarshalJSON(data []byte) error {
	type payload struct {
		MchID         string     `json:"mchid"`
		OutTradeNo    string     `json:"out_trade_no"`
		TransactionID string     `json:"transaction_id"`
		OutRefundNo   string     `json:"out_refund_no"`
		RefundID      string     `json:"refund_id"`
		RefundStatus  string     `json:"refund_status"`
		SuccessTime   *time.Time `json:"success_time"`
		Amount        struct {
			Total  int64 `json:"total"`
			Refund int64 `json:"refund"`
		} `json:"amount"`
	}
	var value payload
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	r.MchID, r.OutTradeNo, r.TransactionID, r.OutRefundNo, r.RefundID, r.RefundStatus, r.SuccessTime = value.MchID, value.OutTradeNo, value.TransactionID, value.OutRefundNo, value.RefundID, value.RefundStatus, value.SuccessTime
	r.Amount.Total, r.Amount.Refund = value.Amount.Total, value.Amount.Refund
	return nil
}

// refundNotificationState 返回退款通知状态。
func refundNotificationState(result *refundNotification) refund.State {
	return refund.State{ProviderRefundID: result.RefundID, RefundNo: result.OutRefundNo, PaymentNo: result.OutTradeNo, Status: result.RefundStatus, Amount: result.Amount.Refund, TotalAmount: result.Amount.Total, SucceededAt: result.SuccessTime}
}

func apiRequestID(result *core.APIResult) string {
	if result == nil || result.Response == nil {
		return ""
	}
	return strings.TrimSpace(result.Response.Header.Get("Request-Id"))
}

func providerCallError(operation string, result *core.APIResult, err error) error {
	providerErr := &paygateway.ProviderError{Operation: operation, RequestID: apiRequestID(result), Retryable: true, Cause: err}
	var apiErr *core.APIError
	if !errors.As(err, &apiErr) {
		providerErr.Message = "provider transport failure"
		return providerErr
	}
	providerErr.HTTPStatus = apiErr.StatusCode
	providerErr.Code = apiErr.Code
	providerErr.Message = apiErr.Message
	if providerErr.RequestID == "" {
		providerErr.RequestID = strings.TrimSpace(apiErr.Header.Get("Request-Id"))
	}
	providerErr.Retryable = apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= http.StatusInternalServerError
	switch apiErr.Code {
	case "SYSTEM_ERROR", "FREQUENCY_LIMITED":
		providerErr.Retryable = true
	case "INVALID_REQUEST", "SIGN_ERROR", "MCH_NOT_EXISTS", "RESOURCE_NOT_EXISTS", "USER_ACCOUNT_ABNORMAL", "NOT_ENOUGH":
		providerErr.Retryable = false
	}
	return providerErr
}

// refundState 返回退款状态。
func refundState(result *refunddomestic.Refund) refund.State {
	state := refund.State{ProviderRefundID: stringValue(result.RefundId), RefundNo: stringValue(result.OutRefundNo), PaymentNo: stringValue(result.OutTradeNo), CurrencyRequired: true, SucceededAt: result.SuccessTime}
	if result.Status != nil {
		state.Status = string(*result.Status)
	}
	if result.Amount != nil {
		state.Amount = int64Value(result.Amount.Refund)
		state.TotalAmount = int64Value(result.Amount.Total)
		state.Currency = stringValue(result.Amount.Currency)
	}
	return state
}

// transactionState 返回交易状态。
func transactionState(transaction *paymentmodel.Transaction) order.ProviderPaymentState {
	state := order.ProviderPaymentState{
		ProviderTradeNo: stringValue(transaction.TransactionId),
		PaymentNo:       stringValue(transaction.OutTradeNo),
		Status:          stringValue(transaction.TradeState),
	}
	if transaction.Amount != nil {
		state.Amount = int64Value(transaction.Amount.Total)
		state.Currency = stringValue(transaction.Amount.Currency)
	}
	if transaction.SuccessTime != nil {
		if paidAt, err := time.Parse(time.RFC3339, *transaction.SuccessTime); err == nil {
			state.PaidAt = &paidAt
		}
	}
	return state
}

type fakeProvider struct {
	appID   string
	mchID   string
	secret  string
	mu      sync.Mutex
	refunds map[string]refund.State
}

// Code 返回代码。
func (p *fakeProvider) Code() string { return "wechat" }

// Create 创建提供器支付结果。
func (p *fakeProvider) Create(_ context.Context, input order.CreateProviderPaymentInput) (order.ProviderPaymentResult, error) {
	prepayID := "test-prepay-" + input.PaymentNo
	return order.ProviderPaymentResult{
		ProviderPrepayID: prepayID,
		Status:           "NOTPAY",
		ClientPayload: map[string]any{
			"appId":     p.appID,
			"timeStamp": "1700000000",
			"nonceStr":  "test-nonce",
			"package":   "prepay_id=" + prepayID,
			"signType":  "RSA",
			"paySign":   "test-signature",
		},
	}, nil
}

// Query 查询提供器支付状态。
func (p *fakeProvider) Query(_ context.Context, paymentNo string) (order.ProviderPaymentState, error) {
	return order.ProviderPaymentState{PaymentNo: paymentNo, Status: "NOTPAY", Currency: "CNY"}, nil
}

// Close 关闭当前实例并释放相关资源。
func (p *fakeProvider) Close(_ context.Context, _ string) error { return nil }

// ParseCallback 解析回调。
func (p *fakeProvider) ParseCallback(_ context.Context, request *http.Request) (order.PaymentCallbackEvent, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 256*1024+1))
	if err != nil || len(body) > 256*1024 {
		return order.PaymentCallbackEvent{}, errors.New("invalid callback body")
	}
	mac := hmac.New(sha256.New, []byte(p.secret))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(request.Header.Get("X-JXE-Fake-Signature"))), []byte(expected)) {
		return order.PaymentCallbackEvent{}, errors.New("invalid callback signature")
	}
	var payload struct {
		EventID         string `json:"event_id"`
		ProviderTradeNo string `json:"provider_trade_no"`
		PaymentNo       string `json:"payment_no"`
		Status          string `json:"status"`
		Amount          int64  `json:"amount"`
		Currency        string `json:"currency"`
		PaidAt          string `json:"paid_at"`
		AppID           string `json:"app_id"`
		MchID           string `json:"mch_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.EventID == "" || payload.PaymentNo == "" {
		return order.PaymentCallbackEvent{}, errors.New("invalid callback payload")
	}
	var paidAt *time.Time
	if payload.PaidAt != "" {
		parsed, err := time.Parse(time.RFC3339, payload.PaidAt)
		if err != nil {
			return order.PaymentCallbackEvent{}, errors.New("invalid callback paid_at")
		}
		paidAt = &parsed
	}
	return order.PaymentCallbackEvent{
		EventID:         payload.EventID,
		ProviderTradeNo: payload.ProviderTradeNo,
		PaymentNo:       payload.PaymentNo,
		Status:          payload.Status,
		Amount:          payload.Amount,
		Currency:        payload.Currency,
		PaidAt:          paidAt,
		AppID:           payload.AppID,
		MchID:           payload.MchID,
	}, nil
}

// Shutdown 平滑停止当前实例并释放相关资源。
func (p *fakeProvider) Shutdown() {}

// Refund 返回退款。
func (p *fakeProvider) Refund(_ context.Context, input refund.Input) (refund.State, error) {
	now := time.Now()
	state := refund.State{ProviderRefundID: "test-refund-" + input.RefundNo, RefundNo: input.RefundNo, PaymentNo: input.PaymentNo, Status: "SUCCESS", Currency: input.Currency, Amount: input.Amount, TotalAmount: input.TotalAmount, CurrencyRequired: true, SucceededAt: &now}
	p.mu.Lock()
	p.refunds[input.RefundNo] = state
	p.mu.Unlock()
	return state, nil
}

// QueryRefund 查询退款。
func (p *fakeProvider) QueryRefund(_ context.Context, refundNo string) (refund.State, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.refunds[refundNo]
	if !ok {
		return refund.State{}, errors.New("refund not found")
	}
	return state, nil
}

// ParseRefundCallback 解析退款回调。
func (p *fakeProvider) ParseRefundCallback(_ context.Context, request *http.Request) (refund.CallbackEvent, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 256*1024+1))
	if err != nil || len(body) > 256*1024 {
		return refund.CallbackEvent{}, errors.New("invalid callback body")
	}
	mac := hmac.New(sha256.New, []byte(p.secret))
	_, _ = mac.Write(body)
	if !hmac.Equal([]byte(strings.ToLower(request.Header.Get("X-JXE-Fake-Signature"))), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return refund.CallbackEvent{}, errors.New("invalid callback signature")
	}
	var payload struct {
		EventID string       `json:"event_id"`
		AppID   string       `json:"app_id"`
		MchID   string       `json:"mch_id"`
		State   refund.State `json:"state"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.EventID == "" || payload.State.RefundNo == "" {
		return refund.CallbackEvent{}, errors.New("invalid callback payload")
	}
	return refund.CallbackEvent{EventID: payload.EventID, AppID: payload.AppID, MchID: payload.MchID, State: payload.State}, nil
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// int64Value 返回int 64 值。
func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
