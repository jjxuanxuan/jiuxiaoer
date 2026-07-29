package core

import (
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

var (
	ShanghaiLocation  = time.FixedZone("Asia/Shanghai", 8*60*60)
	businessNoPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func NowShanghai(now func() time.Time) time.Time {
	return now().In(ShanghaiLocation).Truncate(time.Millisecond)
}

func FormatShanghai(value time.Time) string {
	return value.In(ShanghaiLocation).Format(time.RFC3339Nano)
}

func OptionalTimeString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := FormatShanghai(*value)
	return &formatted
}

func IDString(value uint64) string { return strconv.FormatUint(value, 10) }

func OptionalIDString(value *uint64) *string {
	if value == nil {
		return nil
	}
	formatted := IDString(*value)
	return &formatted
}

func ValidateBusinessNo(value, field string) error {
	if len(value) < 1 ||
		len(value) > 64 ||
		!businessNoPattern.MatchString(value) {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"invalid "+field,
		)
	}
	return nil
}

func JSONData(value any) datatypes.JSON {
	payload, _ := json.Marshal(value)
	return datatypes.JSON(payload)
}

func CloneJSON(value datatypes.JSON) datatypes.JSON {
	if value == nil {
		return nil
	}
	return append(datatypes.JSON(nil), value...)
}
