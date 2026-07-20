package riderapplication

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestSubmitValidationNormalizesSinglePhoneAndScope 验证Submit 校验 Normalizes Single 手机号 And 范围的预期行为。
func TestSubmitValidationNormalizesSinglePhoneAndScope(t *testing.T) {
	req, shops, err := (SubmitRequest{
		Name: " 张三 ", Phone: "13800138000", Code: "123456",
		ServiceScope: ServiceScope{ShopIDs: []string{"42", "7"}},
	}).normalized(50)
	if err != nil {
		t.Fatal(err)
	}
	if req.Name != "张三" || req.Phone != "13800138000" {
		t.Fatalf("unexpected normalized identity: %+v", req)
	}
	if len(shops) != 2 || shops[0] != 7 || shops[1] != 42 || strings.Join(req.ServiceScope.ShopIDs, ",") != "7,42" {
		t.Fatalf("unexpected normalized shops: ids=%v scope=%v", shops, req.ServiceScope.ShopIDs)
	}
}

// TestSubmitValidationRejectsDuplicateAndInvalidFields 验证Submit 校验 Rejects 重复项 And 无效 Fields的预期行为。
func TestSubmitValidationRejectsDuplicateAndInvalidFields(t *testing.T) {
	base := SubmitRequest{
		Name: "张三", Phone: "13800138000", Code: "123456",
		ServiceScope: ServiceScope{ShopIDs: []string{"42"}},
	}
	cases := map[string]SubmitRequest{
		"phone":     func() SubmitRequest { value := base; value.Phone = "123"; return value }(),
		"code":      func() SubmitRequest { value := base; value.Code = "12345"; return value }(),
		"duplicate": func() SubmitRequest { value := base; value.ServiceScope.ShopIDs = []string{"42", "42"}; return value }(),
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := request.normalized(50); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// TestPageTokenBindsFilterAndOrder 验证分页令牌 Binds 筛选条件 And 订单的预期行为。
func TestPageTokenBindsFilterAndOrder(t *testing.T) {
	cfg := config.Load()
	service := NewService(cfg, nil, nil, nil)
	cursor := pageCursor{Filter: "status=submitted", Order: defaultApplicationOrder, LastSubmittedAt: time.Now().Format(time.RFC3339Nano), ID: 9}
	token := service.encodePageToken(cursor)
	decoded, err := service.decodePageToken(token)
	if err != nil || decoded.Filter != cursor.Filter || decoded.Order != cursor.Order || decoded.ID != cursor.ID {
		t.Fatalf("page token round trip failed: decoded=%+v err=%v", decoded, err)
	}
	if _, err := service.decodePageToken(token + "tampered"); err == nil {
		t.Fatal("tampered page token must be rejected")
	}
}

// TestApplicationDTOContainsNoPasswordOrRawTokenFields 验证申请DTO Contains 无密码 Or Raw 令牌 Fields的预期行为。
func TestApplicationDTOContainsNoPasswordOrRawTokenFields(t *testing.T) {
	raw, err := json.Marshal(ApplicationDTO{ID: "1", Phone: "138****8000", Status: StatusSubmitted})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"password", "password_hash", "access_token", "refresh_token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("DTO leaked forbidden field %q: %s", forbidden, text)
		}
	}
}

// TestSubmitRequestHashUsesServerHMACIncludingSMSCode 验证公开申请使用服务端 HMAC 保护请求摘要。
func TestSubmitRequestHashUsesServerHMACIncludingSMSCode(t *testing.T) {
	cfg := config.Load()
	service := NewService(cfg, nil, nil, nil)
	request := SubmitRequest{Name: "张三", Phone: "13800138000", Code: "123456", ServiceScope: ServiceScope{ShopIDs: []string{"4201"}}}
	first := service.hmacJSON(request)
	if first == "" || first != service.hmacJSON(request) {
		t.Fatal("same normalized request must have a stable HMAC")
	}
	changed := request
	changed.Code = "654321"
	if first == service.hmacJSON(changed) {
		t.Fatal("sms code changes must change the protected request hash")
	}
	if first == idempotency.RequestHash(request) {
		t.Fatal("public application request hash must not be an unkeyed digest")
	}
}

// TestPublicSubmitFailsClosedWithoutRedis 验证公开数据 Submit Fails Closed Without Redis的预期行为。
func TestPublicSubmitFailsClosedWithoutRedis(t *testing.T) {
	cfg := config.Load()
	cfg.RiderApplication.Enabled = true
	service := NewService(cfg, &gorm.DB{}, nil, snowflake.New(813))
	_, err := service.Submit(t.Context(), "127.0.0.1", "POST", "/api/v1/rider-applications", "test-idempotency", SubmitRequest{
		Name: "张三", Phone: "13800138000", Code: "123456",
		ServiceScope: ServiceScope{ShopIDs: []string{"4201"}},
	})
	if err == nil || problem.FromError(err).ErrorCode != "DEPENDENCY_UNAVAILABLE" {
		t.Fatalf("public submit must fail closed when Redis is unavailable, got %v", err)
	}
}
