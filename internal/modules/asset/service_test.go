package asset

import (
	"testing"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// TestAssetTypeUnitMatrix 验证资产类型与单位矩阵。
func TestAssetTypeUnitMatrix(t *testing.T) {
	tests := []struct{ assetType, unit string }{{TypeGrowth, UnitPoint}, {TypeWineCoin, UnitPoint}, {TypeBalance, UnitCNY}}
	for _, test := range tests {
		unit, err := UnitFor(test.assetType)
		if err != nil || unit != test.unit {
			t.Fatalf("UnitFor(%s)=%s,%v", test.assetType, unit, err)
		}
	}
	if _, err := UnitFor("coupon"); problem.FromError(err).ErrorCode != "ASSET_TYPE_INVALID" {
		t.Fatalf("unexpected invalid type error: %v", err)
	}
}

// TestSourceRegistryRejectsUnregisteredAndInvalidCompensation 验证来源注册表拒绝未注册来源和无效补偿。
func TestSourceRegistryRejectsUnregisteredAndInvalidCompensation(t *testing.T) {
	s := NewService(config.Load(), nil, nil)
	base := Command{CustomerID: 1, AssetType: TypeBalance, Unit: UnitCNY, Amount: 1, SourceType: "unknown", SourceID: "source-1", Action: "credit", IdempotencyKey: "source-key-1"}
	if err := s.validateCommand(base, 1); problem.FromError(err).ErrorCode != "ASSET_SOURCE_NOT_ALLOWED" {
		t.Fatalf("unexpected unknown source error: %v", err)
	}
	base.SourceType = "compensation"
	base.AssetType, base.Unit = TypeWineCoin, UnitPoint
	if err := s.validateCommand(base, 1); problem.FromError(err).ErrorCode != "ASSET_SOURCE_NOT_ALLOWED" {
		t.Fatalf("unexpected compensation error: %v", err)
	}
}

// TestAmountValidationRejectsZeroAndUnitMismatch 验证金额校验拒绝零值和单位不匹配。
func TestAmountValidationRejectsZeroAndUnitMismatch(t *testing.T) {
	s := NewService(config.Load(), nil, nil)
	cmd := Command{CustomerID: 1, AssetType: TypeBalance, Unit: UnitPoint, Amount: 1, SourceType: "reconciliation_test", SourceID: "source-1", Action: "credit", IdempotencyKey: "source-key-1"}
	if err := s.validateCommand(cmd, 1); problem.FromError(err).ErrorCode != "ASSET_TYPE_INVALID" {
		t.Fatalf("unexpected unit mismatch: %v", err)
	}
	cmd.Unit, cmd.Amount = UnitCNY, 0
	if err := s.validateCommand(cmd, 1); problem.FromError(err).ErrorCode != "ASSET_AMOUNT_INVALID" {
		t.Fatalf("unexpected amount error: %v", err)
	}
}
