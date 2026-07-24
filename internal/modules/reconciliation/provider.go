package reconciliation

import (
	"context"
	"time"
)

// Provider 申请并流式读取官方微信支付 APIv3 账单。
type Provider interface {
	OpenBill(context.Context, time.Time, string) (BillFile, error)
}
