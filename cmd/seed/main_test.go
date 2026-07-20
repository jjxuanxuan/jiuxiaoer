package main

import "testing"

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
