package integrity

import (
	"testing"

	"jiuxiaoer-admin/backend-go/internal/testutil"
)

func uniqueSQLiteMemoryDSN(t *testing.T, options ...string) string {
	t.Helper()
	return testutil.UniqueSQLiteMemoryDSN(t, options...)
}
