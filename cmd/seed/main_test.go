package main

import (
	"strings"
	"testing"
)

func TestValidatePermissionCatalogRejectsIDCodeConflicts(t *testing.T) {
	expected := []permissionSeed{
		{ID: 2132, Code: "cart:view"},
		{ID: 2143, Code: "delivery:force_complete_request"},
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

func TestForceCompleteRolePermissionSplit(t *testing.T) {
	byRole := make(map[uint64]map[uint64]bool)
	for _, assignment := range rolePermissionAssignments() {
		byRole[assignment.roleID] = make(map[uint64]bool, len(assignment.permissionIDs))
		for _, permissionID := range assignment.permissionIDs {
			byRole[assignment.roleID][permissionID] = true
		}
	}

	tests := []struct {
		roleID      uint64
		wantRequest bool
		wantApprove bool
	}{
		{roleID: roleOperation, wantRequest: true, wantApprove: false},
		{roleID: roleAdminManager, wantRequest: false, wantApprove: true},
		{roleID: roleSuperAdmin, wantRequest: true, wantApprove: true},
	}
	for _, tt := range tests {
		permissions := byRole[tt.roleID]
		if got := permissions[2143]; got != tt.wantRequest {
			t.Errorf("role %d request permission = %t, want %t", tt.roleID, got, tt.wantRequest)
		}
		if got := permissions[2144]; got != tt.wantApprove {
			t.Errorf("role %d approve permission = %t, want %t", tt.roleID, got, tt.wantApprove)
		}
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
