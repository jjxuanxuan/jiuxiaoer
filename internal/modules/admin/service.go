package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	repo    *Repository
	redis   *goredis.Client
	idStore *idempotency.Store
	idGen   *snowflake.Generator
}

// NewService 负责平台后台操作，具备全局数据视图。
func NewService(db *gorm.DB, redisClient *goredis.Client, idGen *snowflake.Generator) *Service {
	return &Service{repo: NewRepository(db), redis: redisClient, idStore: idempotency.NewStore(db), idGen: idGen}
}

// CreateProduct 管理平台商品目录，不管理商户门店货架。
func (s *Service) CreateProduct(ctx context.Context, claims *auth.Claims, method string, path string, key string, req ProductCreateReq) (ProductDTO, error) {
	//权限校验
	adminID, err := requireAdminPermission(claims, "product:create")
	if err != nil {
		return ProductDTO{}, err
	}
	categoryID, err := parseID(req.CategoryID)
	if err != nil {
		return ProductDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid category_id")
	}
	status := req.Status
	if status == "" {
		status = "draft"
	}
	originalPrice := req.OriginalPriceAmount
	if originalPrice == 0 {
		originalPrice = req.SalePriceAmount
	}
	row := Product{
		ID:                    s.idGen.Next(),
		CategoryID:            categoryID,
		Name:                  req.Name,
		BrandName:             optionalString(req.BrandName),
		Spec:                  optionalString(req.Spec),
		ImageURL:              optionalString(req.ImageURL),
		Description:           optionalString(req.Description),
		SalePriceAmount:       req.SalePriceAmount,
		OriginalPriceAmount:   originalPrice,
		Status:                status,
		ReturnEligible:        req.ReturnEligible,
		ReturnPolicyCode:      defaultString(req.ReturnPolicyCode, "not_configured"),
		ReturnPolicyVersion:   defaultString(req.ReturnPolicyVersion, "1"),
		SealedPackageRequired: req.SealedPackageRequired,
		AgeRestricted:         req.AgeRestricted,
	}
	var resp ProductDTO
	//开启事务
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		//检查幂等性
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, adminID, path, key, &resp)
		}
		//判断分类是否存在
		if err := s.repo.RequireCategory(ctx, tx, categoryID); errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("CATEGORY_NOT_FOUND", "category not found")
		} else if err != nil {
			return err
		}
		//创建商品
		if err := s.repo.CreateProduct(ctx, tx, row); err != nil {
			return err
		}
		//写后台操作日志
		if err := s.createAudit(ctx, tx, adminID, "product.create", "product", row.ID, nil, row); err != nil {
			return err
		}
		//下游事件投递
		if err := s.createOutbox(ctx, tx, "product.created", "product", row.ID, map[string]any{"product_id": idString(row.ID)}); err != nil {
			return err
		}
		resp = productDTO(row)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, resp)
	})
	return resp, err
}

// UpdateProduct 修改平台级商品目录字段，并在提交后失效公共详情缓存。
func (s *Service) UpdateProduct(ctx context.Context, claims *auth.Claims, method string, path string, key string, productIDRaw string, req ProductUpdateReq) (ProductDTO, error) {
	adminID, err := requireAdminPermission(claims, "product:update")
	if err != nil {
		return ProductDTO{}, err
	}
	productID, err := parseID(productIDRaw)
	if err != nil {
		return ProductDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid product id")
	}
	//构造要变更的map
	values, err := productUpdateValues(req)
	if err != nil {
		return ProductDTO{}, err
	}
	if len(values) == 0 {
		return ProductDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "no fields to update")
	}

	var resp ProductDTO

	// 开启事务
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		//幂等
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.ResourceRequestHash("product.update", productID, req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, adminID, path, key, &resp)
		}
		//如果变更了新类别，判断新类别是否存在
		if categoryIDValue, ok := values["category_id"].(uint64); ok {
			if err := s.repo.RequireCategory(ctx, tx, categoryIDValue); errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.NotFound("CATEGORY_NOT_FOUND", "category not found")
			} else if err != nil {
				return err
			}
		}
		//先加锁，确保审计前后数据和最终提交顺序一致。
		//双重悲观锁保证更新后的数据与构造响应一致
		//悲观锁，取更新前的，当前读
		before, err := s.repo.LockProduct(ctx, tx, productID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("PRODUCT_NOT_FOUND", "product not found")
		}
		if err != nil {
			return err
		}
		//执行更新
		if err := s.repo.UpdateProduct(ctx, tx, productID, values); err != nil {
			return err
		}
		//再锁一次取更新后的
		after, err := s.repo.LockProduct(ctx, tx, productID)
		if err != nil {
			return err
		}
		//记录操作
		if err := s.createAudit(ctx, tx, adminID, "product.update", "product", productID, before, values); err != nil {
			return err
		}
		//事件投递
		if err := s.createOutbox(ctx, tx, "product.updated", "product", productID, map[string]any{"product_id": idString(productID)}); err != nil {
			return err
		}
		if err := s.createCacheInvalidation(ctx, tx, productID, "product_updated"); err != nil {
			return err
		}
		//构造响应
		resp = productDTO(after)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, resp)
	})
	if err == nil {
		s.invalidateProductCache(ctx, productID)
	}
	return resp, err
}

// 更新商品发售状态
func (s *Service) SetProductStatus(ctx context.Context, claims *auth.Claims, method string, path string, key string, productIDRaw string, status string) (ProductDTO, error) {
	return s.UpdateProduct(ctx, claims, method, path, key, productIDRaw, ProductUpdateReq{Status: &status})
}

// ListOrders 为具备 order:list 权限的 admin 提供全局订单视图。
func (s *Service) ListOrders(ctx context.Context, claims *auth.Claims, query pagination.Query, status string) ([]AdminOrderDTO, string, error) {
	if _, err := requireAdminPermission(claims, "order:list"); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.ListOrders(ctx, status, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		nextPageToken = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]AdminOrderDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, orderDTO(row, nil))
	}
	return items, nextPageToken, nil
}

// GetOrder 获取订单。
func (s *Service) GetOrder(ctx context.Context, claims *auth.Claims, orderIDRaw string) (AdminOrderDTO, error) {
	if _, err := requireAdminPermission(claims, "order:view"); err != nil {
		return AdminOrderDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return AdminOrderDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}
	row, items, err := s.repo.GetOrder(ctx, orderID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AdminOrderDTO{}, problem.NotFound("ORDER_NOT_FOUND", "order not found")
	}
	if err != nil {
		return AdminOrderDTO{}, err
	}
	return orderDTO(row, items), nil
}

// ListStocks 查询Stocks列表。
func (s *Service) ListStocks(ctx context.Context, claims *auth.Claims, query pagination.Query, shopIDRaw string) ([]StockDTO, string, error) {
	if _, err := requireAdminPermission(claims, "inventory:view"); err != nil {
		return nil, "", err
	}
	var shopID uint64
	var err error
	if shopIDRaw != "" {
		shopID, err = parseID(shopIDRaw)
		if err != nil {
			return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid shop_id")
		}
	}
	rows, err := s.repo.ListStocks(ctx, shopID, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		nextPageToken = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]StockDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, stockDTO(row))
	}
	return items, nextPageToken, nil
}

// AdjustStock 执行平台级库存调整。
// 规则与商户调整类似，但使用 admin 权限和审计主体。
func (s *Service) AdjustStock(ctx context.Context, claims *auth.Claims, method string, path string, key string, req StockAdjustReq) (StockDTO, error) {
	adminID, err := requireAdminPermission(claims, "inventory:adjust")
	if err != nil {
		return StockDTO{}, err
	}
	if req.QuantityDelta == 0 {
		return StockDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "quantity_delta cannot be zero")
	}
	shopProductID, err := parseID(req.ShopProductID)
	if err != nil {
		return StockDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop_product_id")
	}

	var resp StockDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, adminID, path, key, &resp)
		}
		// 计算前锁定库存行，确保平台和商户库存调整串行化。
		stock, err := s.repo.LockStockByShopProduct(ctx, tx, shopProductID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("STOCK_NOT_FOUND", "stock not found")
		}
		if err != nil {
			return err
		}
		afterAvailable := stock.AvailableQty + req.QuantityDelta
		beforeTotal := stock.AvailableQty + stock.ReservedQty + stock.LockedQty
		if afterAvailable < 0 {
			return problem.Conflict("STOCK_NOT_ENOUGH", "available stock cannot be negative")
		}
		if err := s.repo.AdjustStock(ctx, tx, stock.ID, req.QuantityDelta); err != nil {
			return err
		}
		if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{
			ID:                 s.idGen.Next(),
			ShopProductID:      stock.ShopProductID,
			ShopID:             stock.ShopID,
			ProductID:          stock.ProductID,
			ChangeType:         "admin_adjust",
			QuantityDelta:      req.QuantityDelta,
			BeforeAvailableQty: stock.AvailableQty,
			AfterAvailableQty:  afterAvailable,
			TotalQuantityDelta: req.QuantityDelta,
			BeforeTotalQty:     beforeTotal,
			AfterTotalQty:      beforeTotal + req.QuantityDelta,
			SourceType:         "admin_adjust",
			SourceID:           adminID,
		}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, adminID, "stock.adjust", "shop_product", shopProductID, stock, map[string]any{"available_qty": afterAvailable, "reason": req.Reason}); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "stock.adjusted", "shop_product", shopProductID, map[string]any{"shop_product_id": idString(shopProductID), "quantity_delta": req.QuantityDelta}); err != nil {
			return err
		}
		if err := s.createCacheInvalidation(ctx, tx, stock.ProductID, "stock_adjusted"); err != nil {
			return err
		}
		resp = StockDTO{
			ID:                idString(stock.ID),
			ShopProductID:     idString(stock.ShopProductID),
			ShopID:            idString(stock.ShopID),
			ProductID:         idString(stock.ProductID),
			AvailableQty:      afterAvailable,
			ReservedQty:       stock.ReservedQty,
			LockedQty:         stock.LockedQty,
			LowStockThreshold: stock.LowStockThreshold,
			Version:           stock.Version + 1,
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, resp)
	})
	if err == nil {
		s.invalidateProductCache(ctx, parseProductIDFromStockDTO(resp))
	}
	return resp, err
}

// ListMerchants 查询Merchants列表。
func (s *Service) ListMerchants(ctx context.Context, claims *auth.Claims, query pagination.Query, reviewStatus string) ([]MerchantDTO, string, error) {
	if _, err := requireAdminPermission(claims, "merchant:list"); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.ListMerchants(ctx, reviewStatus, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		nextPageToken = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]MerchantDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, merchantDTO(row))
	}
	return items, nextPageToken, nil
}

// ReviewMerchant 更新商户入驻审核状态，并记录后台审核人。
func (s *Service) ReviewMerchant(ctx context.Context, claims *auth.Claims, method string, path string, key string, merchantIDRaw string, req MerchantReviewReq) (MerchantDTO, error) {
	adminID, err := requireAdminPermission(claims, "merchant:review")
	if err != nil {
		return MerchantDTO{}, err
	}
	merchantID, err := parseID(merchantIDRaw)
	if err != nil {
		return MerchantDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid merchant id")
	}
	var resp MerchantDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.ResourceRequestHash("merchant.review", merchantID, req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, adminID, path, key, &resp)
		}
		before, err := s.repo.LockMerchant(ctx, tx, merchantID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("MERCHANT_NOT_FOUND", "merchant not found")
		}
		if err != nil {
			return err
		}
		if before.ReviewStatus != "pending" {
			return problem.Conflict("MERCHANT_INVALID_STATUS", "merchant review status is not pending")
		}
		now := time.Now()
		values := map[string]any{
			"review_status": req.ReviewStatus,
			"review_remark": req.ReviewRemark,
			"reviewed_at":   &now,
		}
		if req.ReviewStatus == "approved" {
			values["status"] = "active"
		}
		if err := s.repo.UpdateMerchant(ctx, tx, merchantID, values); err != nil {
			return err
		}
		after := before
		after.ReviewStatus = req.ReviewStatus
		after.ReviewRemark = optionalString(req.ReviewRemark)
		after.ReviewedAt = &now
		if value, ok := values["status"].(string); ok {
			after.Status = value
		}
		if err := s.createAudit(ctx, tx, adminID, "merchant.review", "merchant", merchantID, before, values); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "merchant.reviewed", "merchant", merchantID, map[string]any{"merchant_id": idString(merchantID), "review_status": req.ReviewStatus}); err != nil {
			return err
		}
		resp = merchantDTO(after)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, resp)
	})
	return resp, err
}

// cachedResponse 返回缓存响应。
func (s *Service) cachedResponse(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, path string, key string, out any) error {
	cached, err := s.idStore.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if err != nil {
		return err
	}
	if cached {
		return nil
	}
	return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
}

// ListAuditLogs 查询审计 Logs列表。
func (s *Service) ListAuditLogs(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]AuditLogDTO, string, error) {
	if _, err := requireAdminPermission(claims, "audit_log:view"); err != nil {
		return nil, "", err
	}
	if query.OrderBy != "" && query.OrderBy != "created_at desc,id desc" {
		return nil, "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "audit logs use fixed order created_at desc,id desc")
	}
	rows, err := s.repo.ListAuditLogs(ctx, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		last := rows[len(rows)-1]
		nextPageToken = pagination.NextPageTokenWithCursor(query, last.CreatedAt.UTC().Format(time.RFC3339Nano), idString(last.ID))
	}
	items := make([]AuditLogDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, auditLogDTO(row))
	}
	return items, nextPageToken, nil
}

// productUpdateValues 返回商品 Update Values。
func productUpdateValues(req ProductUpdateReq) (map[string]any, error) {
	values := map[string]any{}
	if req.CategoryID != nil {
		categoryID, err := parseID(*req.CategoryID)
		if err != nil {
			return nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid category_id")
		}
		values["category_id"] = categoryID
	}
	if req.Name != nil {
		values["name"] = *req.Name
	}
	if req.BrandName != nil {
		values["brand_name"] = nullableString(*req.BrandName)
	}
	if req.Spec != nil {
		values["spec"] = nullableString(*req.Spec)
	}
	if req.ImageURL != nil {
		values["image_url"] = nullableString(*req.ImageURL)
	}
	if req.Description != nil {
		values["description"] = nullableString(*req.Description)
	}
	if req.SalePriceAmount != nil {
		values["sale_price_amount"] = *req.SalePriceAmount
	}
	if req.OriginalPriceAmount != nil {
		values["original_price_amount"] = *req.OriginalPriceAmount
	}
	if req.Status != nil {
		values["status"] = *req.Status
	}
	if req.ReturnEligible != nil {
		values["return_eligible"] = *req.ReturnEligible
	}
	if req.ReturnPolicyCode != nil {
		values["return_policy_code"] = *req.ReturnPolicyCode
	}
	if req.ReturnPolicyVersion != nil {
		values["return_policy_version"] = *req.ReturnPolicyVersion
	}
	if req.SealedPackageRequired != nil {
		values["sealed_package_required"] = *req.SealedPackageRequired
	}
	if req.AgeRestricted != nil {
		values["age_restricted"] = *req.AgeRestricted
	}
	return values, nil
}

// defaultString 在字符串为空时返回默认值。
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// requireAdminPermission 是后台接口使用的平台 RBAC 入口。
func requireAdminPermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	adminID, err := parseID(claims.AdminUserID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	for _, code := range claims.Permissions {
		if code == permission {
			return adminID, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

// createOutbox 在状态变更同一事务内保存后台侧事件。
func (s *Service) createOutbox(ctx context.Context, tx *gorm.DB, eventType string, aggregateType string, aggregateID uint64, payload any) error {
	return s.repo.CreateOutbox(ctx, tx, OutboxEvent{
		ID:            s.idGen.Next(),
		EventID:       uuid.NewString(),
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Payload:       jsonData(payload),
		Status:        "pending",
		RetryCount:    0,
		RequestID:     requestctx.RequestIDPtr(ctx),
	})
}

// createCacheInvalidation 创建缓存 Invalidation。
func (s *Service) createCacheInvalidation(ctx context.Context, tx *gorm.DB, productID uint64, reason string) error {
	pattern := "product:detail:" + idString(productID) + "*"
	return s.createOutbox(ctx, tx, "cache.invalidate", "product", productID, map[string]any{"patterns": []string{pattern}, "reason": reason})
}

// createAudit 记录平台后台写操作，便于复核和追责。
func (s *Service) createAudit(ctx context.Context, tx *gorm.DB, actorID uint64, action string, resourceType string, resourceID uint64, before any, after any) error {
	return s.repo.CreateAuditLog(ctx, tx, AuditLog{
		ID:           s.idGen.Next(),
		ActorType:    "admin",
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		BeforeData:   jsonData(before),
		AfterData:    jsonData(after),
		Result:       "success",
		RequestID:    requestctx.RequestIDPtr(ctx),
		IPHash:       requestctx.IPHashPtr(ctx),
		UserAgent:    requestctx.UserAgentPtr(ctx),
	})
}

// invalidateProductCache 遵循和商户写操作相同的 cache-aside 规则。
// 可靠 cache.invalidate 已在业务事务内写入；这里的同步删除只用于降低读旧值窗口。
func (s *Service) invalidateProductCache(ctx context.Context, productID uint64) {
	if s.redis == nil || productID == 0 {
		return
	}
	pattern := "product:detail:" + idString(productID) + "*"
	var cursor uint64
	var keys []string
	var err error
	for {
		var batch []string
		var next uint64
		batch, next, err = s.redis.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			break
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if err == nil && len(keys) > 0 {
		err = s.redis.Del(ctx, keys...).Err()
	}
	_ = s.redis.Incr(ctx, "home_version:global").Err()
}

// productDTO 返回商品DTO。
func productDTO(row Product) ProductDTO {
	return ProductDTO{
		ID:                    idString(row.ID),
		CategoryID:            idString(row.CategoryID),
		Name:                  row.Name,
		BrandName:             stringValue(row.BrandName),
		Spec:                  stringValue(row.Spec),
		ImageURL:              stringValue(row.ImageURL),
		Description:           stringValue(row.Description),
		SalePriceAmount:       row.SalePriceAmount,
		OriginalPriceAmount:   row.OriginalPriceAmount,
		Status:                row.Status,
		ReturnEligible:        row.ReturnEligible,
		ReturnPolicyCode:      row.ReturnPolicyCode,
		ReturnPolicyVersion:   row.ReturnPolicyVersion,
		SealedPackageRequired: row.SealedPackageRequired,
		AgeRestricted:         row.AgeRestricted,
	}
}

// orderDTO 返回订单DTO。
func orderDTO(row Order, items []OrderItem) AdminOrderDTO {
	dto := AdminOrderDTO{
		ID:                idString(row.ID),
		OrderNo:           row.OrderNo,
		CustomerID:        idString(row.CustomerID),
		MerchantID:        idString(row.MerchantID),
		ShopID:            idString(row.ShopID),
		Status:            row.Status,
		PayStatus:         row.PayStatus,
		DeliveryStatus:    row.DeliveryStatus,
		GoodsAmount:       row.GoodsAmount,
		DiscountAmount:    row.DiscountAmount,
		DeliveryFeeAmount: row.DeliveryFeeAmount,
		PayableAmount:     row.PayableAmount,
		PaidAmount:        row.PaidAmount,
		CreatedAt:         row.CreatedAt.Format(time.RFC3339),
	}
	for _, item := range items {
		dto.Items = append(dto.Items, orderItemDTO(item))
	}
	return dto
}

// orderItemDTO 返回订单明细DTO。
func orderItemDTO(row OrderItem) OrderItemDTO {
	var snapshot map[string]string
	_ = json.Unmarshal(row.ProductSnapshot, &snapshot)
	return OrderItemDTO{
		ID:              idString(row.ID),
		ShopProductID:   idString(row.ShopProductID),
		ProductID:       idString(row.ProductID),
		Name:            snapshot["name"],
		Quantity:        row.Quantity,
		SalePriceAmount: row.SalePriceAmount,
		TotalAmount:     row.TotalAmount,
	}
}

// stockDTO 返回库存DTO。
func stockDTO(row StockRow) StockDTO {
	return StockDTO{
		ID:                idString(row.ID),
		ShopProductID:     idString(row.ShopProductID),
		ShopID:            idString(row.ShopID),
		MerchantID:        idString(row.MerchantID),
		ProductID:         idString(row.ProductID),
		ProductName:       row.ProductName,
		AvailableQty:      row.AvailableQty,
		ReservedQty:       row.ReservedQty,
		LockedQty:         row.LockedQty,
		LowStockThreshold: row.LowStockThreshold,
		Version:           row.Version,
	}
}

// merchantDTO 返回商户DTO。
func merchantDTO(row Merchant) MerchantDTO {
	return MerchantDTO{
		ID:           idString(row.ID),
		Code:         row.Code,
		Name:         row.Name,
		ContactName:  stringValue(row.ContactName),
		ContactPhone: stringValue(row.ContactPhone),
		LicenseNo:    stringValue(row.LicenseNo),
		Status:       row.Status,
		ReviewStatus: row.ReviewStatus,
		ReviewRemark: stringValue(row.ReviewRemark),
		CreatedAt:    row.CreatedAt.Format(time.RFC3339),
	}
}

// auditLogDTO 返回审计日志DTO。
func auditLogDTO(row AuditLog) AuditLogDTO {
	return AuditLogDTO{
		ID:           idString(row.ID),
		EventID:      stringValue(row.EventID),
		ActorType:    row.ActorType,
		ActorID:      idString(row.ActorID),
		AccountID:    optionalIDString(row.AccountID),
		Action:       row.Action,
		ResourceType: row.ResourceType,
		ResourceID:   idString(row.ResourceID),
		ShopID:       optionalIDString(row.ShopID),
		OrderID:      optionalIDString(row.OrderID),
		DeliveryID:   optionalIDString(row.DeliveryID),
		BeforeData:   rawJSON(row.BeforeData),
		AfterData:    rawJSON(row.AfterData),
		Result:       row.Result,
		ErrorCode:    stringValue(row.ErrorCode),
		ReasonCode:   stringValue(row.ReasonCode),
		BeforeStatus: stringValue(row.BeforeStatus),
		AfterStatus:  stringValue(row.AfterStatus),
		Version:      optionalUint64(row.Version),
		RequestID:    stringValue(row.RequestID),
		IPHash:       stringValue(row.IPHash),
		UserAgent:    stringValue(row.UserAgent),
		CreatedAt:    row.CreatedAt.Format(time.RFC3339),
	}
}

func optionalIDString(value *uint64) string {
	if value == nil || *value == 0 {
		return ""
	}
	return idString(*value)
}

func optionalUint64(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

// rawJSON 将输入值序列化为原始 JSON 数据。
func rawJSON(value datatypes.JSON) json.RawMessage {
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return json.RawMessage(value)
}

// parseProductIDFromStockDTO 从库存 DTO 中解析商品 ID。
func parseProductIDFromStockDTO(dto StockDTO) uint64 {
	id, _ := strconv.ParseUint(dto.ProductID, 10, 64)
	return id
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, problem.InvalidArgument("VALIDATION_FAILED", "invalid id")
	}
	return id, nil
}

// idString 将数字 ID 转换为字符串。
func idString(id uint64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatUint(id, 10)
}

// optionalString 将空字符串转换为空指针，否则返回字符串指针。
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// nullableString 将空字符串转换为数据库空值，否则返回原字符串。
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(value any) datatypes.JSON {
	if value == nil {
		return nil
	}
	payload, _ := json.Marshal(value)
	return datatypes.JSON(payload)
}
