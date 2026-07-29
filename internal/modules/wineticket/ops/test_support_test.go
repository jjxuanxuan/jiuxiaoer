package ops

import (
	"net/url"
	"strings"
	"testing"
)

func uniqueSQLiteMemoryDSN(t *testing.T, options ...string) string {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	query := url.Values{"cache": {"shared"}, "mode": {"memory"}}
	for _, option := range options {
		key, value, ok := strings.Cut(option, "=")
		if ok {
			query.Set(key, value)
		}
	}
	return "file:" + name + "?" + query.Encode()
}
