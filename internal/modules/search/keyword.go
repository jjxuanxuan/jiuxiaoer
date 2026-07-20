package search

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

var (
	phonePattern      = regexp.MustCompile(`(^|[^0-9])1[3-9][0-9]{9}([^0-9]|$)`)
	identityPattern   = regexp.MustCompile(`(^|[^0-9])([0-9]{15}|[0-9]{17}[0-9Xx])([^0-9A-Za-z]|$)`)
	emailPattern      = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	longNumberPattern = regexp.MustCompile(`[0-9]{8,}`)
)

func normalizeKeyword(raw string) (display string, normalized string, err error) {
	value := norm.NFKC.String(raw)
	for _, current := range value {
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return "", "", problem.InvalidArgument("SEARCH_KEYWORD_INVALID", "keyword contains unsupported characters")
		}
	}
	value = strings.Join(strings.Fields(value), " ")
	if value == "" || utf8.RuneCountInString(value) > 64 {
		return "", "", problem.InvalidArgument("SEARCH_KEYWORD_INVALID", "keyword must contain between 1 and 64 characters")
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for _, current := range value {
		if current >= 'A' && current <= 'Z' {
			current += 'a' - 'A'
		}
		builder.WriteRune(current)
	}
	return value, builder.String(), nil
}

func validSource(value string) bool {
	return value == SourceManual || value == SourceHistory || value == SourceHot
}

func eligibleForHot(normalized string, blocklist map[string]struct{}) bool {
	if _, blocked := blocklist[normalized]; blocked {
		return false
	}
	return !phonePattern.MatchString(normalized) &&
		!identityPattern.MatchString(normalized) &&
		!emailPattern.MatchString(normalized) &&
		!longNumberPattern.MatchString(normalized)
}

func keywordHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}
