package shop

import (
	"context"
	"strconv"

	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Service struct {
	repo  *Repository
	redis *goredis.Client
}

// NewService 创建并初始化服务。
func NewService(db *gorm.DB, redisClient *goredis.Client) *Service {
	return &Service{repo: NewRepository(db), redis: redisClient}
}

// ListPublicShops 查询公开数据 Shops列表。
func (s *Service) ListPublicShops(ctx context.Context, query ListQuery) ([]ShopDTO, string, error) {
	shops, err := s.repo.ListPublicShops(ctx, query)
	if err != nil {
		return nil, "", err
	}

	nextPageToken := ""
	if len(shops) > query.PageSize {
		nextPageToken = pagination.NextPageToken(query.Query)
		shops = shops[:query.PageSize]
	}

	items := make([]ShopDTO, 0, len(shops))
	for _, row := range shops {
		items = append(items, ShopDTO{
			ID:               strconv.FormatUint(row.ID, 10),
			MerchantID:       strconv.FormatUint(row.MerchantID, 10),
			Name:             row.Name,
			Phone:            stringValue(row.Phone),
			City:             row.City,
			CityCode:         stringValue(row.CityCode),
			District:         row.District,
			Address:          row.Address,
			Latitude:         floatValue(row.Latitude),
			Longitude:        floatValue(row.Longitude),
			CoordinateSystem: row.CoordinateSystem,
			Status:           row.Status,
			BusinessStatus:   row.BusinessStatus,
			DistanceM:        row.DistanceM, Serviceable: row.Serviceable,
			DeliveryFeeAmount:           row.DeliveryFeeAmount,
			FreeDeliveryThresholdAmount: row.FreeDeliveryThresholdAmount,
			DeliveryETAMin:              row.DeliveryETAMin, DeliveryETAMax: row.DeliveryETAMax,
			OvertimePolicyCode: stringValue(row.OvertimePolicyCode),
		})
	}
	return items, nextPageToken, nil
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// floatValue 返回float 值。
func floatValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}
