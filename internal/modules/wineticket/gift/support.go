package gift

import (
	"strings"
	"time"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
)

var shanghaiLocation = core.ShanghaiLocation

func idString(value uint64) string { return core.IDString(value) }

func formatShanghai(value time.Time) string { return core.FormatShanghai(value) }

func optionalTimeString(value *time.Time) *string {
	return core.OptionalTimeString(value)
}

func validateBusinessNo(value, field string) error {
	return core.ValidateBusinessNo(strings.TrimSpace(value), field)
}

func jsonData(value any) datatypes.JSON { return core.JSONData(value) }
