package provisioning

import (
	"reflect"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestNormalizeRiderCreate(t *testing.T) {
	req, shopIDs, err := normalizeRiderCreate(RiderCreateReq{
		Name:  " 管理员创建骑手 ",
		Phone: " 13800138000 ",
		ServiceScope: map[string]any{
			"shop_ids": []any{"4202", "4201"},
		},
	})
	if err != nil {
		t.Fatalf("normalize rider create: %v", err)
	}
	if req.Phone != "13800138000" || req.Name != "管理员创建骑手" {
		t.Fatalf("unexpected normalized identity: %#v", req)
	}
	if !reflect.DeepEqual(shopIDs, []uint64{4201, 4202}) {
		t.Fatalf("unexpected shop ids: %#v", shopIDs)
	}
	if !reflect.DeepEqual(req.ServiceScope["shop_ids"], []string{"4201", "4202"}) {
		t.Fatalf("unexpected normalized scope: %#v", req.ServiceScope)
	}
}

func TestNormalizeRiderCreateRejectsInvalidInput(t *testing.T) {
	valid := RiderCreateReq{
		Name:  "管理员创建骑手",
		Phone: "13800138000",
		ServiceScope: map[string]any{
			"shop_ids": []any{"4201"},
		},
	}
	tests := map[string]func(*RiderCreateReq){
		"invalid phone":  func(req *RiderCreateReq) { req.Phone = "123" },
		"decimal shop":   func(req *RiderCreateReq) { req.ServiceScope = map[string]any{"shop_ids": []any{4201.5}} },
		"duplicate shop": func(req *RiderCreateReq) { req.ServiceScope = map[string]any{"shop_ids": []any{"4201", "4201"}} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			req := valid
			mutate(&req)
			_, _, err := normalizeRiderCreate(req)
			if err == nil || problem.FromError(err).ErrorCode != "VALIDATION_FAILED" {
				t.Fatalf("expected validation failure, got %v", err)
			}
		})
	}
}
