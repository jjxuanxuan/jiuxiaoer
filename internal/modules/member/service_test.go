package member

import "testing"

// TestEvaluateTierBoundaries 验证会员等级计算边界。
func TestEvaluateTierBoundaries(t *testing.T) {
	rules := []Rule{{TierCode: "normal", TierName: "普通会员", MinGrowth: 0}, {TierCode: "silver", TierName: "银卡会员", MinGrowth: 1000}, {TierCode: "gold", TierName: "金卡会员", MinGrowth: 5000}}
	tests := []struct {
		growth     int64
		tier, next string
		remaining  int64
	}{{0, "normal", "silver", 1000}, {999, "normal", "silver", 1}, {1000, "silver", "gold", 4000}, {4999, "silver", "gold", 1}, {5000, "gold", "", 0}}
	for _, test := range tests {
		tier, next, remaining := evaluate(test.growth, rules)
		nextCode := ""
		if next != nil {
			nextCode = next.TierCode
		}
		if tier.TierCode != test.tier || nextCode != test.next || remaining != test.remaining {
			t.Fatalf("growth=%d got tier=%s next=%s remaining=%d", test.growth, tier.TierCode, nextCode, remaining)
		}
	}
}
