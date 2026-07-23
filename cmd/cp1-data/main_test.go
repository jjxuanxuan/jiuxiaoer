package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/modules/cp1data"
)

func TestParseTemplateMap(t *testing.T) {
	got, err := parseTemplateMap("12:9001, 13:9002")
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint64]uint64{12: 9001, 13: 9002}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("map = %#v, want %#v", got, want)
	}
	if _, err := parseTemplateMap("12:9001,12:9002"); err == nil {
		t.Fatal("conflicting mapping was accepted")
	}
}

func TestParseOptionalTimeRequiresRFC3339(t *testing.T) {
	value, err := parseOptionalTime("2026-07-22T10:00:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	if value == nil || !value.Equal(want) {
		t.Fatalf("cutover = %v, want %v", value, want)
	}
	if _, err := parseOptionalTime("2026-07-22 10:00:00"); err == nil {
		t.Fatal("ambiguous local time was accepted")
	}
}

func TestLoadVerificationAuditRejectsDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	if err := cp1data.SaveReport(path, cp1data.VerificationAudit{
		SchemaVersion: "cp1.verification-migration-audit.v1",
		DryRun:        true,
		Completed:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVerificationAudit(path); err == nil {
		t.Fatal("dry-run migration audit was accepted as DQ evidence")
	}
}
