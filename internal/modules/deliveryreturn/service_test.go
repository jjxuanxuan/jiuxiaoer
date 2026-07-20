package deliveryreturn

import (
	"errors"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestCreateIsDisabledByDefault(t *testing.T) {
	t.Parallel()
	service := NewService(config.Load(), nil, nil, nil)
	_, err := service.Create(t.Context(), riderClaims("42"), "POST", "/api/v1/delivery/orders/:id/returns", "request-key-0001", "100", validCreateReq())
	assertProblemCode(t, err, "DELIVERY_RETURN_DISABLED")
}

func TestCreateEnforcesPermissionAndRolloutBeforeStorage(t *testing.T) {
	t.Parallel()
	cfg := returnTestConfig()
	service := NewService(cfg, nil, nil, nil)

	claims := riderClaims("42")
	claims.Permissions = nil
	_, err := service.Create(t.Context(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "request-key-0002", "100", validCreateReq())
	assertProblemCode(t, err, "RETURN_FORBIDDEN")

	claims = riderClaims("43")
	_, err = service.Create(t.Context(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "request-key-0003", "100", validCreateReq())
	assertProblemCode(t, err, "RETURN_FORBIDDEN")
}

func TestCreateValidatesHighRiskReasonsBeforeStorage(t *testing.T) {
	t.Parallel()
	cfg := returnTestConfig()
	service := NewService(cfg, nil, nil, nil)
	claims := riderClaims("42")

	req := validCreateReq()
	req.ReasonCode = "not-real"
	_, err := service.Create(t.Context(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "request-key-0004", "100", req)
	assertProblemCode(t, err, "INVALID_REASON_CODE")

	req = validCreateReq()
	req.ReasonCode = ReasonOther
	_, err = service.Create(t.Context(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "request-key-0005", "100", req)
	assertProblemCode(t, err, "VALIDATION_FAILED")

	req = validCreateReq()
	req.ReasonCode = ReasonDamagedInTransit
	_, err = service.Create(t.Context(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "request-key-0006", "100", req)
	assertProblemCode(t, err, "VALIDATION_FAILED")
}

func TestCreateFailsClosedWithoutRedis(t *testing.T) {
	t.Parallel()
	cfg := returnTestConfig()
	cfg.DeliveryReturn.RiderAllowlist = []string{"*"}
	service := NewService(cfg, nil, nil, nil)
	_, err := service.Create(t.Context(), riderClaims("42"), "POST", "/api/v1/delivery/orders/:id/returns", "request-key-0007", "100", validCreateReq())
	assertProblemCode(t, err, "DELIVERY_RETURN_DEPENDENCY_UNAVAILABLE")
}

func TestOrthogonalBranchProjection(t *testing.T) {
	t.Parallel()
	service := &Service{cfg: returnTestConfig()}
	requested := Return{ID: 1, Status: StatusRequested}
	if got := service.dto(Aggregate{Return: requested}, "rider"); got.RefundStatus != "not_authorized" || got.InventoryStatus != "not_applicable" || got.LogisticsStatus != StatusRequested {
		t.Fatalf("unexpected requested projection: %+v", got)
	}
	afterSaleID := uint64(7)
	received := Return{ID: 2, Status: StatusReceived, AfterSaleID: &afterSaleID}
	if got := service.dto(Aggregate{Return: received}, "rider"); got.RefundStatus != "processing" || got.InventoryStatus != "disposed" || got.LogisticsStatus != StatusReceived {
		t.Fatalf("unexpected received projection: %+v", got)
	}
	closed := Return{ID: 3, Status: StatusClosed, AfterSaleID: &afterSaleID}
	if got := service.dto(Aggregate{Return: closed}, "rider"); got.RefundStatus != "succeeded" || got.InventoryStatus != "disposed" || got.LogisticsStatus != StatusReceived {
		t.Fatalf("unexpected closed projection: %+v", got)
	}
}

func validCreateReq() CreateReq {
	return CreateReq{ReasonCode: ReasonCustomerRefused, ExpectedDeliveryVersion: 1}
}

func riderClaims(id string) *auth.Claims {
	return &auth.Claims{AccountType: "rider", RiderID: id, Permissions: []string{"delivery_return:create", "delivery_return:view_own"}}
}

func returnTestConfig() config.Config {
	cfg := config.Load()
	cfg.DeliveryReturn.Enabled = true
	cfg.DeliveryReturn.RiderWriteEnabled = true
	cfg.DeliveryReturn.RiderAllowlist = []string{"42"}
	return cfg
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var details *problem.Details
	if !errors.As(err, &details) || details.ErrorCode != code {
		t.Fatalf("expected problem %s, got %v", code, err)
	}
}
