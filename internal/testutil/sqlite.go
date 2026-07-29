package testutil

import (
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
)

var sqliteMemoryDatabaseSequence atomic.Uint64

type testNamer interface {
	Helper()
	Name() string
}

// UniqueSQLiteMemoryDSN 防止同一进程中的测试连接到
// 其他测试或 -count 迭代遗留的共享内存数据库。
func UniqueSQLiteMemoryDSN(t testNamer, options ...string) string {
	t.Helper()
	query := "mode=memory&cache=shared"
	if len(options) > 0 {
		query += "&" + strings.Join(options, "&")
	}
	return fmt.Sprintf(
		"file:test-%s-%d?%s",
		url.QueryEscape(t.Name()),
		sqliteMemoryDatabaseSequence.Add(1),
		query,
	)
}
