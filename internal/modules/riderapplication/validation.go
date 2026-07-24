package riderapplication

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

var (
	phonePattern = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
	codePattern  = regexp.MustCompile(`^[0-9]{6}$`)
)

// normalized 返回规范化后的提交请求。
func (r SubmitRequest) normalized(maxShops int) (SubmitRequest, []uint64, error) {
	r.Name = strings.TrimSpace(r.Name)
	r.Phone = strings.TrimSpace(r.Phone)
	if err := validateName(r.Name); err != nil {
		return SubmitRequest{}, nil, err
	}
	if !phonePattern.MatchString(r.Phone) {
		return SubmitRequest{}, nil, invalid("phone must be a mainland China mobile number")
	}
	if !codePattern.MatchString(r.Code) {
		return SubmitRequest{}, nil, invalid("code must be a 6 digit sms code")
	}
	shops, normalizedScope, err := normalizeScope(r.ServiceScope, maxShops)
	if err != nil {
		return SubmitRequest{}, nil, err
	}
	r.ServiceScope = normalizedScope
	return r, shops, nil
}

// normalized 返回规范化后的登录请求。
func (r LoginRequest) normalized() (LoginRequest, error) {
	r.Phone = strings.TrimSpace(r.Phone)
	if !phonePattern.MatchString(r.Phone) || !codePattern.MatchString(r.Code) {
		return LoginRequest{}, invalid("invalid phone or sms code")
	}
	return r, nil
}

// normalized 返回规范化后的审核请求。
func (r UpdateRequest) normalized(maxShops int) (UpdateRequest, []uint64, error) {
	r.Name = strings.TrimSpace(r.Name)
	if err := validateName(r.Name); err != nil {
		return UpdateRequest{}, nil, err
	}
	if r.ExpectedVersion == 0 {
		return UpdateRequest{}, nil, invalid("expected_version must be positive")
	}
	shops, normalizedScope, err := normalizeScope(r.ServiceScope, maxShops)
	if err != nil {
		return UpdateRequest{}, nil, err
	}
	r.ServiceScope = normalizedScope
	return r, shops, nil
}

// validate 校验riderapplication是否合法。
func (r VersionRequest) validate() error {
	if r.ExpectedVersion == 0 {
		return invalid("expected_version must be positive")
	}
	return nil
}

// normalized 返回规范化后的服务范围。
func (r ReviewRequest) normalized() (ReviewRequest, error) {
	r.Decision = strings.ToLower(strings.TrimSpace(r.Decision))
	r.Reason = strings.TrimSpace(r.Reason)
	if r.Decision != StatusApproved && r.Decision != StatusRejected {
		return ReviewRequest{}, invalid("decision must be approved or rejected")
	}
	if utf8.RuneCountInString(r.Reason) < 2 || utf8.RuneCountInString(r.Reason) > 255 {
		return ReviewRequest{}, invalid("reason must be between 2 and 255 characters")
	}
	if r.ExpectedVersion == 0 {
		return ReviewRequest{}, invalid("expected_version must be positive")
	}
	return r, nil
}

// validateName 校验Name是否合法。
func validateName(value string) error {
	length := utf8.RuneCountInString(value)
	if length < 1 || length > 64 {
		return invalid("name must be between 1 and 64 characters")
	}
	return nil
}

// normalizeScope 规范化范围。
func normalizeScope(scope ServiceScope, maxShops int) ([]uint64, ServiceScope, error) {
	if len(scope.ShopIDs) < 1 || len(scope.ShopIDs) > maxShops {
		return nil, ServiceScope{}, invalid(fmt.Sprintf("service_scope.shop_ids must contain 1 to %d items", maxShops))
	}
	seen := make(map[uint64]struct{}, len(scope.ShopIDs))
	ids := make([]uint64, 0, len(scope.ShopIDs))
	for _, raw := range scope.ShopIDs {
		id, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if err != nil || id == 0 {
			return nil, ServiceScope{}, invalid("service_scope.shop_ids must contain positive integer strings")
		}
		if _, exists := seen[id]; exists {
			return nil, ServiceScope{}, invalid("service_scope.shop_ids must not contain duplicates")
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	normalized := ServiceScope{ShopIDs: make([]string, 0, len(ids))}
	for _, id := range ids {
		normalized.ShopIDs = append(normalized.ShopIDs, strconv.FormatUint(id, 10))
	}
	return ids, normalized, nil
}

// invalid 返回无效。
func invalid(detail string) error {
	return problem.InvalidArgument("VALIDATION_FAILED", detail)
}
