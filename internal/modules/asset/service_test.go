package asset

import (
	"testing"

	"github.com/gin-gonic/gin"

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

// TestSourceActionDirectionMatrix 验证人工调账方向严格匹配且受控冻结来源仍合法。
func TestSourceActionDirectionMatrix(t *testing.T) {
	s := NewService(config.Load(), nil, nil)
	base := Command{
		CustomerID:     1,
		AssetType:      TypeWineCoin,
		Unit:           UnitPoint,
		Amount:         1,
		SourceType:     "reconciliation_test",
		SourceID:       "source-1",
		IdempotencyKey: "source-key-1",
	}

	freeze := base
	freeze.Action = "freeze"
	if err := s.validateCommand(freeze, -1); err != nil {
		t.Fatalf("controlled freeze must remain valid: %v", err)
	}

	invalidFreeze := freeze
	invalidFreeze.SourceType = "manual_adjustment"
	if err := s.validateCommand(invalidFreeze, -1); problem.FromError(err).ErrorCode != "ASSET_SOURCE_NOT_ALLOWED" {
		t.Fatalf("manual adjustment freeze must be rejected: %v", err)
	}

	mismatchedDebit := base
	mismatchedDebit.SourceType = "manual_adjustment"
	mismatchedDebit.Action = "credit"
	if err := s.validateCommand(mismatchedDebit, -1); problem.FromError(err).ErrorCode != "ASSET_SOURCE_NOT_ALLOWED" {
		t.Fatalf("manual adjustment direction mismatch must be rejected: %v", err)
	}
}

// TestAdminAdjustmentRoutesAreSingleStep 验证人工调账只保留创建即执行路由。
func TestAdminAdjustmentRoutesAreSingleStep(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAdminRoutes(router.Group("/api/v1/admin"), NewHandler(nil))

	foundCreate := false
	for _, route := range router.Routes() {
		switch route.Path {
		case "/api/v1/admin/asset-adjustments":
			foundCreate = route.Method == "POST"
		case "/api/v1/admin/asset-adjustments/:id/approve", "/api/v1/admin/asset-adjustments/:id/reject":
			t.Fatalf("obsolete adjustment review route is still registered: %s %s", route.Method, route.Path)
		}
	}
	if !foundCreate {
		t.Fatal("single-step adjustment route is not registered")
	}
}

// TestAdjustmentExecutionErrorPreservesFailureCode 验证失败重放保持稳定错误码。
func TestAdjustmentExecutionErrorPreservesFailureCode(t *testing.T) {
	for _, code := range []string{"ASSET_INSUFFICIENT_AVAILABLE", "ASSET_WRITE_DISABLED", "ASSET_AMOUNT_INVALID", "INTERNAL_ERROR"} {
		err := adjustmentExecutionError(code)
		if got := problem.FromError(err).ErrorCode; got != code {
			t.Fatalf("adjustmentExecutionError(%q) code=%q", code, got)
		}
	}
}
