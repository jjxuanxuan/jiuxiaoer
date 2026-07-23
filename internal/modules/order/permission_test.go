package order

import (
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestCustomerOrderAndPaymentPermissionsAreOperationSpecific(t *testing.T) {
	claims := &auth.Claims{AccountType: "customer", CustomerID: "100", Permissions: []string{"order:view"}}
	if id, err := customerIDFromClaims(claims, "order:view"); err != nil || id != 100 {
		t.Fatalf("expected exact permission to pass, id=%d err=%v", id, err)
	}
	if _, err := customerIDFromClaims(claims, "payment:view"); problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("expected missing payment:view to fail closed, got %v", err)
	}
	if _, err := customerCancelActorFromClaims(claims); problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("expected missing order:cancel to fail closed, got %v", err)
	}
	admin := &auth.Claims{AccountType: "admin", AdminUserID: "200", Permissions: []string{"order:cancel_all"}}
	if _, err := customerCancelActorFromClaims(admin); problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("admin token must not authorize the customer cancel entrypoint, got %v", err)
	}
	if actor, err := adminCancelActorFromClaims(admin); err != nil || !actor.IsAdmin || actor.ActorID != 200 {
		t.Fatalf("expected exact admin cancellation permission to pass, actor=%+v err=%v", actor, err)
	}
}
