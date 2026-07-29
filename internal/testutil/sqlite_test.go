package testutil

import (
	"strings"
	"testing"
)

func TestUniqueSQLiteMemoryDSN(t *testing.T) {
	first := UniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")
	second := UniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")

	if first == second {
		t.Fatalf("DSNs must be unique: %q", first)
	}
	for _, dsn := range []string{first, second} {
		if !strings.Contains(dsn, "mode=memory&cache=shared") {
			t.Fatalf("DSN is not a shared in-memory database: %q", dsn)
		}
		if !strings.Contains(dsn, "_busy_timeout=5000") {
			t.Fatalf("DSN dropped options: %q", dsn)
		}
	}
}
