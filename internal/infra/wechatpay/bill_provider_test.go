package wechatpay

import (
	"net/url"
	"testing"
	"time"

	"github.com/wechatpay-apiv3/wechatpay-go/services/refunddomestic"
)

func TestBillDownloadURLIsRestrictedToOfficialHTTPSHost(t *testing.T) {
	for _, raw := range []string{"http://api.mch.weixin.qq.com/v3/billdownload/file", "https://example.com/v3/billdownload/file", "https://user@api.mch.weixin.qq.com/v3/billdownload/file", "https://api.mch.weixin.qq.com:444/v3/billdownload/file", "https://api.mch.weixin.qq.com/v3/billdownload/file#fragment", "https://api.mch.weixin.qq.com/v3/other/file"} {
		parsed, _ := url.Parse(raw)
		if err := validateBillDownloadURL(parsed); err == nil {
			t.Fatalf("expected unsafe URL to fail: %s", raw)
		}
	}
	for _, raw := range []string{"https://api.mch.weixin.qq.com/v3/billdownload/file?token=opaque", "https://api.mch.weixin.qq.com/v3/bill/downloadurl?token=opaque"} {
		parsed, _ := url.Parse(raw)
		if err := validateBillDownloadURL(parsed); err != nil {
			t.Fatalf("expected official download URL %s: %v", raw, err)
		}
	}
}

func TestRefundStatePreservesProviderCreateTimeForDailyBills(t *testing.T) {
	createdAt := time.Date(2026, 7, 19, 23, 59, 0, 0, time.FixedZone("CST", 8*60*60))
	refundID, refundNo, paymentNo, status := "5031", "RF1", "PAY1", refunddomestic.STATUS_SUCCESS
	state := refundState(&refunddomestic.Refund{RefundId: &refundID, OutRefundNo: &refundNo, OutTradeNo: &paymentNo, Status: &status, CreateTime: &createdAt})
	if state.ProviderCreatedAt == nil || !state.ProviderCreatedAt.Equal(createdAt) {
		t.Fatalf("provider create time lost: %+v", state)
	}
}
