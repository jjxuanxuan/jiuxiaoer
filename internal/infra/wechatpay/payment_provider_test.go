package wechatpay

import (
	"testing"

	paymentmodel "github.com/wechatpay-apiv3/wechatpay-go/services/payments"
)

func TestTransactionStatePreservesIdentityAndOptionalAmountPresence(t *testing.T) {
	appid, mchid, paymentNo, status := "wx-app", "1900000001", "PAY1", "NOTPAY"
	state := transactionState(&paymentmodel.Transaction{Appid: &appid, Mchid: &mchid, OutTradeNo: &paymentNo, TradeState: &status})
	if state.AppID != appid || state.MchID != mchid || state.PaymentNo != paymentNo || state.AmountPresent || state.Amount != 0 || state.Currency != "" {
		t.Fatalf("optional query amount was not preserved: %+v", state)
	}
	total, currency := int64(1), "CNY"
	state = transactionState(&paymentmodel.Transaction{Appid: &appid, Mchid: &mchid, OutTradeNo: &paymentNo, TradeState: &status, Amount: &paymentmodel.TransactionAmount{Total: &total, Currency: &currency}})
	if !state.AmountPresent || state.Amount != total || state.Currency != currency {
		t.Fatalf("present query amount was lost: %+v", state)
	}
}
