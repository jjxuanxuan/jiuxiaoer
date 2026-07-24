package order

import "testing"

// TestBusinessNumbersKeepFullSnowflakeID 验证业务单号保留完整雪花 ID。
func TestBusinessNumbersKeepFullSnowflakeID(t *testing.T) {
	const id uint64 = 1234567890123456789
	if got := orderNo(id); got != "JXE1234567890123456789" {
		t.Fatalf("unexpected order number: %s", got)
	}
	if got := paymentNo(id); got != "PAY1234567890123456789" {
		t.Fatalf("unexpected payment number: %s", got)
	}
}
