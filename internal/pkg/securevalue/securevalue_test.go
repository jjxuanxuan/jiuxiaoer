package securevalue

import "testing"

// TestSealAndHMAC 验证Seal And HMAC的预期行为。
func TestSealAndHMAC(t *testing.T) {
	key := "01234567890123456789012345678901"
	sealed, err := Seal(key, "sensitive")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(key, sealed)
	if err != nil || opened != "sensitive" {
		t.Fatalf("round trip failed: value=%q err=%v", opened, err)
	}
	one := HMAC(key, "delivery", "pickup", "123456")
	two := HMAC(key, "delivery", "pickup", "123456")
	if !EqualHMAC(one, two) || EqualHMAC(one, HMAC(key, "delivery", "delivery", "123456")) {
		t.Fatal("HMAC comparison did not preserve stage separation")
	}
}
