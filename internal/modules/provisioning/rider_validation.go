package provisioning

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const maxProvisionedRiderShops = 50

var (
	provisionedRiderPhonePattern = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
)

func normalizeRiderCreate(req RiderCreateReq) (RiderCreateReq, []uint64, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.Phone = strings.TrimSpace(req.Phone)
	nameLength := utf8.RuneCountInString(req.Name)
	if nameLength < 1 || nameLength > 64 {
		return RiderCreateReq{}, nil, problem.InvalidArgument("VALIDATION_FAILED", "name must be between 1 and 64 characters")
	}
	if !provisionedRiderPhonePattern.MatchString(req.Phone) {
		return RiderCreateReq{}, nil, problem.InvalidArgument("VALIDATION_FAILED", "phone must be a mainland China mobile number")
	}

	shopIDs, err := serviceScopeShopIDs(req.ServiceScope)
	if err != nil {
		return RiderCreateReq{}, nil, problem.InvalidArgument("VALIDATION_FAILED", err.Error())
	}
	sort.Slice(shopIDs, func(i, j int) bool { return shopIDs[i] < shopIDs[j] })
	normalizedShopIDs := make([]string, 0, len(shopIDs))
	for _, shopID := range shopIDs {
		normalizedShopIDs = append(normalizedShopIDs, strconv.FormatUint(shopID, 10))
	}
	req.ServiceScope = map[string]any{"shop_ids": normalizedShopIDs}
	return req, shopIDs, nil
}

// serviceScopeShopIDs 返回服务范围门店 IDs。
func serviceScopeShopIDs(scope map[string]any) ([]uint64, error) {
	raw, ok := scope["shop_ids"]
	if !ok {
		return nil, fmt.Errorf("service_scope.shop_ids is required")
	}
	values := make([]any, 0)
	switch typed := raw.(type) {
	case []any:
		values = typed
	case []string:
		for _, value := range typed {
			values = append(values, value)
		}
	default:
		return nil, fmt.Errorf("service_scope.shop_ids must be an array")
	}
	if len(values) < 1 || len(values) > maxProvisionedRiderShops {
		return nil, fmt.Errorf("service_scope.shop_ids must contain 1 to %d items", maxProvisionedRiderShops)
	}

	result := make([]uint64, 0, len(values))
	seen := map[uint64]bool{}
	for _, rawID := range values {
		var id uint64
		var err error
		switch value := rawID.(type) {
		case string:
			id, err = strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		case float64:
			if value <= 0 || value != math.Trunc(value) || value > math.MaxUint64 {
				err = fmt.Errorf("invalid shop id")
			} else {
				id = uint64(value)
			}
		default:
			err = fmt.Errorf("invalid shop id")
		}
		if err != nil || id == 0 {
			return nil, fmt.Errorf("service_scope.shop_ids must contain positive integer strings")
		}
		if seen[id] {
			return nil, fmt.Errorf("service_scope.shop_ids must not contain duplicates")
		}
		seen[id] = true
		result = append(result, id)
	}
	return result, nil
}
