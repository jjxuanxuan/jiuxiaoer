package main

import (
	"strings"
	"testing"
)

func TestValidatePermissionCatalogRejectsIDCodeConflicts(t *testing.T) {
	expected := []permissionSeed{
		{ID: 2132, Code: "cart:view"},
		{ID: 2068, Code: "delivery:force_complete"},
	}
	tests := []struct {
		name   string
		actual []permissionCatalogRow
	}{
		{name: "reserved id has another code", actual: []permissionCatalogRow{{ID: 2132, Code: "foreign:permission"}}},
		{name: "reserved code has another id", actual: []permissionCatalogRow{{ID: 9999, Code: "cart:view"}}},
		{name: "case variant cannot hide unique code conflict", actual: []permissionCatalogRow{{ID: 9999, Code: "Cart:View"}}},
		{name: "trailing spaces cannot hide unique code conflict", actual: []permissionCatalogRow{{ID: 9999, Code: "cart:view "}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePermissionCatalog(expected, tt.actual)
			if err == nil || !strings.Contains(err.Error(), "permission seed catalog conflict") {
				t.Fatalf("validatePermissionCatalog() error = %v, want catalog conflict", err)
			}
		})
	}
	if err := validatePermissionCatalog(expected, []permissionCatalogRow{{ID: 2132, Code: "cart:view"}}); err != nil {
		t.Fatalf("exact id/code mapping was rejected: %v", err)
	}
}

func TestSingleOperatorAdminPermissions(t *testing.T) {
	byRole := make(map[uint64]map[uint64]bool)
	for _, assignment := range rolePermissionAssignments() {
		byRole[assignment.roleID] = make(map[uint64]bool, len(assignment.permissionIDs))
		for _, permissionID := range assignment.permissionIDs {
			byRole[assignment.roleID][permissionID] = true
		}
	}

	activeMatrix := map[uint64]map[uint64]bool{
		2048: {
			roleSuperAdmin:   true,
			roleAdminManager: true,
			roleFinance:      true,
		},
		2068: {
			roleSuperAdmin:   true,
			roleAdminManager: true,
			roleOperation:    true,
		},
		2148: {
			roleSuperAdmin:   true,
			roleAdminManager: true,
			roleOperation:    true,
		},
		2164: {
			roleSuperAdmin:   true,
			roleAdminManager: true,
			roleOperation:    true,
		},
	}
	for permissionID, expectedRoles := range activeMatrix {
		for _, roleID := range []uint64{
			roleSuperAdmin,
			roleAdminManager,
			roleOperation,
			roleFinance,
			roleCustomerService,
		} {
			if got, want := byRole[roleID][permissionID], expectedRoles[roleID]; got != want {
				t.Errorf(
					"role %d direct permission %d = %t, want %t",
					roleID,
					permissionID,
					got,
					want,
				)
			}
		}
	}
	for roleID, rolePermissions := range byRole {
		for _, retiredID := range []uint64{2049, 2143, 2144, 2149} {
			if rolePermissions[retiredID] {
				t.Errorf("role %d still has retired approval permission %d", roleID, retiredID)
			}
		}
	}

	byCode := make(map[string]bool, len(permissions))
	for _, permission := range permissions {
		byCode[permission.Code] = true
	}
	for _, retiredCode := range []string{
		"asset_adjustment:approve",
		"delivery:force_complete_request",
		"delivery:force_complete_approve",
		"wine_ticket_exception:review",
	} {
		if byCode[retiredCode] {
			t.Errorf("retired permission %q remains in active seed catalog", retiredCode)
		}
	}
}

func TestWineTicketPackagePublishPermissionsStayDistinct(t *testing.T) {
	byCode := make(map[string]permissionSeed, len(permissions))
	for _, permission := range permissions {
		byCode[permission.Code] = permission
	}
	publish, ok := byCode["wine_ticket_package:publish"]
	if !ok || publish.ID != 2164 || publish.Action != "publish" {
		t.Fatalf("publish permission=%+v, want id=2164 action=publish", publish)
	}
	unpublish, ok := byCode["wine_ticket_package:unpublish"]
	if !ok || unpublish.ID != 2186 || unpublish.Action != "unpublish" {
		t.Fatalf("unpublish permission=%+v, want id=2186 action=unpublish", unpublish)
	}

	var operationPermissions map[uint64]bool
	for _, assignment := range rolePermissionAssignments() {
		if assignment.roleID != roleOperation {
			continue
		}
		operationPermissions = make(map[uint64]bool, len(assignment.permissionIDs))
		for _, permissionID := range assignment.permissionIDs {
			operationPermissions[permissionID] = true
		}
	}
	if !operationPermissions[publish.ID] || !operationPermissions[unpublish.ID] {
		t.Fatalf(
			"operation role package permissions publish=%t unpublish=%t",
			operationPermissions[publish.ID],
			operationPermissions[unpublish.ID],
		)
	}
}

func TestMerchantLeastPrivilegeRoleMatrix(t *testing.T) {
	byRole := make(map[uint64]map[uint64]bool)
	for _, assignment := range rolePermissionAssignments() {
		permissions := make(map[uint64]bool, len(assignment.permissionIDs))
		for _, permissionID := range assignment.permissionIDs {
			permissions[permissionID] = true
		}
		byRole[assignment.roleID] = permissions
	}

	owner := byRole[roleMerchantOwner]
	if len(owner) != 25 {
		t.Fatalf("merchant owner permission count=%d, want 25", len(owner))
	}
	for _, required := range []uint64{
		2013, 2138, 2014, 2015,
		2016, 2017, 2018, 2019,
		2005, 2006,
		2027, 2028, 2029, 2030, 2031,
		2053, 2054, 2139, 2055, 2056, 2064,
		2114, 2124, 2125, 2126,
	} {
		if !owner[required] {
			t.Errorf("merchant owner missing permission id %d", required)
		}
	}
	orderOperator := byRole[roleMerchantOrder]
	if len(orderOperator) != 10 {
		t.Fatalf("order operator permission count=%d, want 10", len(orderOperator))
	}
	if !orderOperator[2014] || !orderOperator[2015] {
		t.Errorf("order operator cannot fulfill orders: %v", orderOperator)
	}
	if orderOperator[2006] {
		t.Error("order operator unexpectedly has inventory:adjust")
	}
	stockClerk := byRole[roleMerchantStock]
	if len(stockClerk) != 2 {
		t.Fatalf("inventory clerk permission count=%d, want 2", len(stockClerk))
	}
	if !stockClerk[2005] || !stockClerk[2006] {
		t.Errorf("inventory clerk lacks inventory permissions: %v", stockClerk)
	}
	if stockClerk[2014] || stockClerk[2015] {
		t.Error("inventory clerk unexpectedly has order accept/prepare")
	}
	for _, adminRole := range []uint64{roleSuperAdmin, roleAdminManager, roleOperation} {
		if !byRole[adminRole][2145] {
			t.Errorf("admin role %d cannot update merchant roles", adminRole)
		}
	}
}

func TestCustomerLBSSeedCoversDemoServiceRadiusDistricts(t *testing.T) {
	expected := map[string]string{
		"440300": "city",
		"440304": "district",
		"440305": "district",
	}
	seenCodes := make(map[string]bool, len(customerLBSADCodes))
	seenIDs := make(map[uint64]bool, len(customerLBSADCodes))
	for _, item := range customerLBSADCodes {
		if seenCodes[item.code] || seenIDs[item.id] {
			t.Fatalf("duplicate customer LBS seed: id=%d adcode=%s", item.id, item.code)
		}
		seenCodes[item.code], seenIDs[item.id] = true, true
		if level, ok := expected[item.code]; ok && item.level != level {
			t.Fatalf("adcode %s level=%s, want %s", item.code, item.level, level)
		}
	}
	for adcode := range expected {
		if !seenCodes[adcode] {
			t.Fatalf("customer LBS seed is missing required adcode %s", adcode)
		}
	}
}
