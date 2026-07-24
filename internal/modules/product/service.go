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

const categoryCacheReadAttempts = 3

const (
	contextServiceShop     = "service_shop"
	contextNoServiceShop   = "no_service_shop"
	reasonLocationRequired = "location_required"
	reasonOutOfService     = "out_of_service"
	reasonShopClosed       = "shop_closed"
	reasonNotOnSale        = "not_on_sale"
	reasonOutOfStock       = "out_of_stock"
)

// Service 提供公共商品目录读取，并使用 Redis cache-aside。
type Service struct {
	repo               *Repository
	redis              *goredis.Client
	resolver           *servicearea.Service
	lbsMode            string
	locations          *customerlocation.Service
	resolveShopForTest func(context.Context, *ListQuery) (*servicearea.DeliveryPromiseDTO, error)
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
	if s.redis == nil {
		return s.listCategoriesFromDB(ctx)
	}

	// 返回命中结果或发布缓存填充前重新检查版本。这可消除分类事务在首次读取版本
	// 与访问 Redis 或数据库之间提交所产生的竞争：更新的已提交版本可见后，
	// 绝不会再返回旧版本键。
	for attempt := 0; attempt < categoryCacheReadAttempts; attempt++ {
		revision, err := s.repo.CategoryCatalogRevision(ctx)
		if err != nil {
			return nil, err
		}
		cacheKey := categoryCacheKey(revision)
		if cached, err := s.redis.Get(ctx, cacheKey).Result(); err == nil {
			var items []CategoryDTO
			if json.Unmarshal([]byte(cached), &items) == nil {
				confirmedRevision, confirmErr := s.repo.CategoryCatalogRevision(ctx)
				if confirmErr != nil {
					return nil, confirmErr
				}
				if confirmedRevision == revision {
					return items, nil
				}
				continue
			}
		}

		items, err := s.listCategoriesFromDB(ctx)
		if err != nil {
			return nil, err
		}
		confirmedRevision, err := s.repo.CategoryCatalogRevision(ctx)
		if err != nil {
			return nil, err
		}
		if confirmedRevision != revision {
			continue
		}
		s.setJSONCache(ctx, cacheKey, items, 10*time.Minute)
		return items, nil
	}

	// 分类持续写入时，优先使用新的数据库快照，
	// 而不是填充或返回请求期间版本已变化的缓存记录。
	return s.listCategoriesFromDB(ctx)
}

func categoryCacheKey(revision string) string {
	return "category:list:" + revision
}

func (s *Service) listCategoriesFromDB(ctx context.Context) ([]CategoryDTO, error) {
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
	return items, nil
}

// ListProducts 查询商品列表。
func (s *Service) ListProducts(ctx context.Context, query ListQuery) ([]ProductDTO, string, error) {
	if query.OrderBy != "" || query.Filter != "" {
		return nil, "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "product list has a fixed sort and filter contract")
	}
	promise, err := s.resolveProductShop(ctx, &query)
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
		rows = rows[:query.PageSize]
		last := rows[len(rows)-1]
		switch {
		case query.OrderBy != "":
			nextPageToken = pagination.NextPageToken(query.Query)
		case query.ShopID != "":
			nextPageToken = pagination.NextPageTokenWithCursor(query.Query, strconv.Itoa(last.SortOrder), strconv.FormatUint(last.ShopProductID, 10))
		default:
			nextPageToken = pagination.NextPageTokenWithCursor(query.Query, strconv.FormatUint(last.ID, 10))
		}
	}

	items := make([]ProductDTO, 0, len(rows))
	for _, row := range rows {
		item := productResponse(productDTO(row), query, promise)
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

	promise, err := s.resolveProductShop(ctx, &query)
	if err != nil {
		return ProductDTO{}, err
	}
	shopID := uint64(0)
	if query.ShopID != "" {
		shopID, err = strconv.ParseUint(query.ShopID, 10, 64)
		if err != nil || shopID == 0 {
			return ProductDTO{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid shop_id")
		}
	}
	cacheKey := "product:detail:" + id + ":" + query.ShopID
	if s.redis != nil {
		if cached, err := s.redis.Get(ctx, cacheKey).Result(); err == nil {
			if cached == nullCacheValue {
				return ProductDTO{}, problem.NotFound("PRODUCT_NOT_FOUND", "product not found")
			}
			var item ProductDTO
			if json.Unmarshal([]byte(cached), &item) == nil {
				return productResponse(item, query, promise), nil
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
	item := productDTO(row)
	// 只缓存门店静态商品投影。配送承诺与位置相关，
	// 每次读取缓存后都必须重新附加。
	item.DeliveryPromise = nil
	s.setJSONCache(ctx, cacheKey, item, 5*time.Minute)
	return productResponse(item, query, promise), nil
}

func (s *Service) resolveProductShop(ctx context.Context, query *ListQuery) (*servicearea.DeliveryPromiseDTO, error) {
	if s.resolveShopForTest != nil {
		return s.resolveShopForTest(ctx, query)
	}
	return s.resolveShop(ctx, query)
}

// resolveShop 解析并返回门店。
func (s *Service) resolveShop(ctx context.Context, query *ListQuery) (*servicearea.DeliveryPromiseDTO, error) {
	query.locationlessReason = reasonLocationRequired
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
				query.locationlessReason = reasonLocationRequired
				if query.ShopID != "" && s.lbsMode == "enforce" {
					return nil, problem.New(422, "PRECISE_LOCATION_REQUIRED", "Unprocessable Entity", "a precise location is required for shop inventory")
				}
				query.ShopID = ""
				return nil, nil
			}
			if location.ServiceShop == nil {
				query.locationlessReason = reasonOutOfService
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
		// 上方必需上下文分支始终会返回。即使未来重构改变流程，
		// 此保护仍能明确保留强制执行契约。
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
	item := ProductDTO{
		ID:            strconv.FormatUint(row.ID, 10),
		CategoryID:    strconv.FormatUint(row.CategoryID, 10),
		Name:          row.Name,
		BrandName:     stringValue(row.BrandName),
		Spec:          stringValue(row.Spec),
		ImageURL:      stringValue(row.ImageURL),
		Description:   stringValue(row.Description),
		Status:        row.Status,
		AgeRestricted: row.AgeRestricted,
	}
	if row.ShopID == 0 || row.ShopProductID == 0 {
		item.ContextType = contextNoServiceShop
		item.Purchasable = false
		item.UnavailableReason = stringPtr(reasonLocationRequired)
		return item
	}

	availableQty := row.AvailableQty
	if availableQty < 0 {
		availableQty = 0
	}
	item.ContextType = contextServiceShop
	item.ShopID = strconv.FormatUint(row.ShopID, 10)
	item.ShopProductID = strconv.FormatUint(row.ShopProductID, 10)
	item.SalePriceAmount = int64Ptr(row.SalePriceAmount)
	item.OriginalPriceAmount = int64Ptr(row.OriginalPriceAmount)
	item.AvailableQty = intPtr(availableQty)
	if reason := serviceShopUnavailableReason(row); reason != "" {
		item.UnavailableReason = stringPtr(reason)
		return item
	}
	item.Purchasable = true
	return item
}

func productResponse(item ProductDTO, query ListQuery, promise *servicearea.DeliveryPromiseDTO) ProductDTO {
	// 旧缓存记录可能仍包含位置相关的承诺。附加本次请求解析出的承诺前，
	// 始终先将其清除。
	item.DeliveryPromise = nil
	if query.ShopID == "" {
		item.ContextType = contextNoServiceShop
		item.ShopID = ""
		item.ShopProductID = ""
		item.SalePriceAmount = nil
		item.OriginalPriceAmount = nil
		item.AvailableQty = nil
		item.Purchasable = false
		reason := query.locationlessReason
		if reason == "" {
			reason = reasonLocationRequired
		}
		item.UnavailableReason = stringPtr(reason)
		return item
	}

	item.ContextType = contextServiceShop
	// 契约变更前写入的记录没有判别字段或可用性字段，且只包含可售商品，
	// 因此静态状态和库存投影足以将其规范化。
	if item.UnavailableReason == nil && !item.Purchasable {
		switch {
		case item.Status != "on_sale":
			item.UnavailableReason = stringPtr(reasonNotOnSale)
		case item.AvailableQty != nil && *item.AvailableQty <= 0:
			item.UnavailableReason = stringPtr(reasonOutOfStock)
		default:
			item.Purchasable = true
		}
	}
	if promise != nil {
		item.DeliveryPromise = promise
	}
	return item
}

func serviceShopUnavailableReason(row ProductRow) string {
	if row.ShopStatus != "active" || row.BusinessStatus != "open" {
		return reasonShopClosed
	}
	if row.Status != "on_sale" || row.ShopProductStatus != "on_sale" {
		return reasonNotOnSale
	}
	if row.AvailableQty <= 0 {
		return reasonOutOfStock
	}
	return ""
}

func stringPtr(value string) *string { return &value }

func int64Ptr(value int64) *int64 { return &value }

func intPtr(value int) *int { return &value }

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
