package reconciliation

import (
	"context"
	"time"
)

// Provider applies for and streams official WeChat Pay API v3 bills.
type Provider interface {
	OpenBill(context.Context, time.Time, string) (BillFile, error)
}
