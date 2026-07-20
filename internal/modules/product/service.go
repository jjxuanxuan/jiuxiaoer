package product

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const nullCacheValue = "__NULL__"

// Service 提供公共商品目录读取，并使用 Redis cache-aside。
type Service struct {
	repo      *Repository
	redis     *goredis.Client
	resolver  *servicearea.Service
	lbsMode   string
	locations *customerlocation.Service
}

func (s *Service) WithLocationContexts(mode string, locations *customerlocation.Service) *Service {
	s.lbsMode, s.locations = mode, locations
	return s
}

// NewService 创建并初始化服务。
func NewService(db *gorm.DB, redisClient *goredis.Client, resolvers ...*servicearea.Service) *Service {
	var resolver *servicearea.Service
	if len(resolvers) > 0 {
		resolver = resolvers[0]
	}
	return &Service{repo: NewRepository(db), redis: redisClient, resolver: resolver}
}

// ListCategories 使用较长缓存，因为 P0 阶段分类变化较少。
func (s *Service) ListCategories(ctx context.Context) ([]CategoryDTO, error) {
	const cacheKey = "category:list"
	if s.redis != nil {
		if cached, err := s.redis.Get(ctx, cacheKey).Result(); err == nil {
			var items []CategoryDTO
			if json.Unmarshal([]byte(cached), &items) == nil {
				return items, nil
			}
		}
	}

	categories, err := s.repo.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]CategoryDTO, 0, len(categories))
	for _, category := range categories {
		parentID := ""
		if category.ParentID != nil {
			parentID = strconv.FormatUint(*category.ParentID, 10)
		}
		items = append(items, CategoryDTO{
			ID:            strconv.FormatUint(category.ID, 10),
			ParentID:      parentID,
			Name:          category.Name,
			SortOrder:     category.SortOrder,
			Status:        category.Status,
			AgeRestricted: category.AgeRestricted,
		})
	}

	s.setJSONCache(ctx, cacheKey, items, 10*time.Minute)
	return items, nil
}

// ListProducts 查询商品列表。
func (s *Service) ListProducts(ctx context.Context, query ListQuery) ([]ProductDTO, string, error) {
	promise, err := s.resolveShop(ctx, &query)
	if err != nil {
		return nil, "", err
	}
	for field, raw := range map[string]string{"shop_id": query.ShopID, "category_id": query.CategoryID} {
		if raw == "" {
			continue
		}
		if id, err := strconv.ParseUint(raw, 10, 64); err != nil || id == 0 {
			return nil, "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid "+field)
		}
	}
	rows, err := s.repo.ListProducts(ctx, query)
	if err != nil {
		return nil, "", err
	}

	nextPageToken := ""
	if len(rows) > query.PageSize {
		nextPageToken = pagination.NextPageToken(query.Query)
		rows = rows[:query.PageSize]
	}

	items := make([]ProductDTO, 0, len(rows))
	for _, row := range rows {
		item := productDTO(row)
		if promise != nil {
			item.DeliveryPromise = promise
		}
		items = append(items, item)
	}
	return items, nextPageToken, nil
}

// GetPublicProduct 同时缓存命中结果和短期空值，减少重复 ID 探测。
func (s *Service) GetPublicProduct(ctx context.Context, id string, query ListQuery) (ProductDTO, error) {
	productID, err := strconv.ParseUint(id, 10, 64)
	if err != nil || productID == 0 {
		return ProductDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid product id")
	}

	promise, err := s.resolveShop(ctx, &query)
	if err != nil {
		return ProductDTO{}, err
	}
	shopID, _ := strconv.ParseUint(query.ShopID, 10, 64)
	cacheKey := "product:detail:" + id + ":" + query.ShopID
	if s.redis != nil {
		if cached, err := s.redis.Get(ctx, cacheKey).Result(); err == nil {
			if cached == nullCacheValue {
				return ProductDTO{}, problem.NotFound("PRODUCT_NOT_FOUND", "product not found")
			}
			var item ProductDTO
			if json.Unmarshal([]byte(cached), &item) == nil {
				return item, nil
			}
		}
	}

	row, err := s.repo.GetPublicProduct(ctx, productID, shopID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 空值缓存故意设置得较短，避免新商品长时间不可见。
		s.setStringCache(ctx, cacheKey, nullCacheValue, 60*time.Second)
		return ProductDTO{}, problem.NotFound("PRODUCT_NOT_FOUND", "product not found")
	}
	if err != nil {
		return ProductDTO{}, err
	}
	if shopID != 0 && row.AvailableQty <= 0 {
		return ProductDTO{}, problem.Conflict("PRODUCT_NOT_ON_SALE", "product is unavailable at the service shop")
	}

	item := productDTO(row)
	if promise != nil {
		item.DeliveryPromise = promise
	}
	s.setJSONCache(ctx, cacheKey, item, 5*time.Minute)
	return item, nil
}

// resolveShop 返回resolve 门店。
func (s *Service) resolveShop(ctx context.Context, query *ListQuery) (*servicearea.DeliveryPromiseDTO, error) {
	if s.lbsMode == "enforce" {
		if query.LocationContextID == "" || s.locations == nil {
			return nil, problem.New(422, "LOCATION_CONTEXT_REQUIRED", "Unprocessable Entity", "X-Location-Context is required")
		}
		if query.CityCode != "" || query.Latitude != nil || query.Longitude != nil {
			return nil, problem.Conflict("LOCATION_CONTEXT_CONFLICT", "legacy location query conflicts with location context")
		}
	}
	if query.LocationContextID != "" && s.locations != nil && (s.lbsMode == "observe" || s.lbsMode == "enforce") {
		location, err := s.locations.GetContext(ctx, query.LocationActor, query.LocationContextID)
		if err != nil {
			if s.lbsMode == "enforce" {
				return nil, err
			}
		} else {
			if s.lbsMode == "observe" {
				s.locations.ObserveReadComparison("products", query.CityCode, query.ShopID, location)
			}
			if location.LocationLevel == "city" {
				if query.ShopID != "" && s.lbsMode == "enforce" {
					return nil, problem.New(422, "PRECISE_LOCATION_REQUIRED", "Unprocessable Entity", "a precise location is required for shop inventory")
				}
				query.ShopID = ""
				return nil, nil
			}
			if location.ServiceShop == nil {
				if s.lbsMode == "enforce" {
					return nil, problem.New(422, "OUT_OF_SERVICE_AREA", "Unprocessable Entity", "location is not serviceable")
				}
			} else {
				if query.ShopID != "" && query.ShopID != location.ServiceShop.ID && s.lbsMode == "enforce" {
					return nil, problem.Conflict("SERVICE_SHOP_CHANGED", "requested shop does not match the current location context")
				}
				query.ShopID = location.ServiceShop.ID
				return location.DeliveryPromise, nil
			}
		}
	}
	if s.lbsMode == "enforce" {
		// The required context branch above always returns. This guard keeps the
		// enforcement contract explicit if a future refactor changes that flow.
		return nil, problem.New(422, "LOCATION_CONTEXT_REQUIRED", "Unprocessable Entity", "X-Location-Context is required")
	}
	hasLocation := query.CityCode != "" || query.Latitude != nil || query.Longitude != nil
	if !hasLocation {
		return nil, nil
	}
	if query.CityCode == "" || query.Latitude == nil || query.Longitude == nil || s.resolver == nil {
		return nil, problem.New(422, "LOCATION_REQUIRED", "Unprocessable Entity", "city_code, lat and lng are required together")
	}
	resolved, err := s.resolver.Resolve(ctx, servicearea.ResolveInput{CityCode: query.CityCode, Latitude: *query.Latitude, Longitude: *query.Longitude})
	if err != nil {
		return nil, err
	}
	if query.ShopID != "" && query.ShopID != resolved.ServiceShop.ID {
		return nil, problem.Conflict("SERVICE_SHOP_CHANGED", "requested shop does not match the current service shop")
	}
	query.ShopID = resolved.ServiceShop.ID
	return &resolved.ServiceShop.DeliveryPromise, nil
}

// setJSONCache 是尽力写入；Redis 写失败时读接口仍应成功。
func (s *Service) setJSONCache(ctx context.Context, key string, value any, ttl time.Duration) {
	if s.redis == nil {
		return
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	_ = s.redis.Set(ctx, key, payload, ttl).Err()
}

// setStringCache 设置字符串缓存。
func (s *Service) setStringCache(ctx context.Context, key string, value string, ttl time.Duration) {
	if s.redis == nil {
		return
	}
	_ = s.redis.Set(ctx, key, value, ttl).Err()
}

// productDTO 返回商品DTO。
func productDTO(row ProductRow) ProductDTO {
	return ProductDTO{
		ID:                  strconv.FormatUint(row.ID, 10),
		CategoryID:          strconv.FormatUint(row.CategoryID, 10),
		ShopID:              strconv.FormatUint(row.ShopID, 10),
		ShopProductID:       strconv.FormatUint(row.ShopProductID, 10),
		Name:                row.Name,
		BrandName:           stringValue(row.BrandName),
		Spec:                stringValue(row.Spec),
		ImageURL:            stringValue(row.ImageURL),
		Description:         stringValue(row.Description),
		SalePriceAmount:     row.SalePriceAmount,
		OriginalPriceAmount: row.OriginalPriceAmount,
		Status:              row.Status,
		AvailableQty:        row.AvailableQty,
		AgeRestricted:       row.AgeRestricted,
	}
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
