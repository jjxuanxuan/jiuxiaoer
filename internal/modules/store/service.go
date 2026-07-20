package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	mysqlerr "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	repo     *Repository
	redis    *goredis.Client
	idStore  *idempotency.Store
	idGen    *snowflake.Generator
	cp1      config.CP1Config
	dispatch *dispatch.Service
}

// WithCP1 设置CP 1并返回更新后的值。
func (s *Service) WithCP1(cfg config.CP1Config) *Service { s.cp1 = cfg; return s }

// WithDispatch 设置调度并返回更新后的值。
func (s *Service) WithDispatch(service *dispatch.Service) *Service { s.dispatch = service; return s }

// NewService 负责商户侧订单履约、门店商品和库存操作。
func NewService(db *gorm.DB, redisClient *goredis.Client, idGen *snowflake.Generator) *Service {
	return &Service{
		repo:    NewRepository(db),
		redis:   redisClient,
		idStore: idempotency.NewStore(db),
		idGen:   idGen,
	}
}

// ListOrders 只返回当前商户账号授权门店下的订单。
func (s *Service) ListOrders(ctx context.Context, claims *auth.Claims, query pagination.Query, status string) ([]StoreOrderDTO, string, error) {
	identity, err := merchantIdentityFromClaims(claims)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.repo.ListOrders(ctx, identity.MerchantID, identity.ShopIDs, status, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		nextPageToken = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]StoreOrderDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, storeOrderDTO(row, nil))
	}
	return items, nextPageToken, nil
}

// AcceptOrder 将已支付订单推进到商户已接单状态。
// 带授权范围的订单锁可防止商户接到其他商户的订单。
func (s *Service) AcceptOrder(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string) (StoreOrderDTO, error) {
	identity, orderID, err := s.merchantOrderActionInput(claims, orderIDRaw)
	if err != nil {
		return StoreOrderDTO{}, err
	}
	reqHash := idempotency.RequestHash(map[string]string{"order_id": orderIDRaw, "action": "accept"})

	var resp StoreOrderDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, reqHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}

		row, err := s.repo.LockAuthorizedOrder(ctx, tx, identity.MerchantID, identity.ShopIDs, orderID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ORDER_NOT_FOUND", "order not found")
		}
		if err != nil {
			return err
		}
		if row.Status != "paid" {
			return problem.Conflict("ORDER_INVALID_STATUS", "only paid orders can be accepted")
		}
		if err := s.repo.UpdateOrder(ctx, tx, orderID, map[string]any{"status": "accepted"}); err != nil {
			return err
		}
		if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{
			ID:         s.idGen.Next(),
			OrderID:    orderID,
			ActorType:  claims.AccountType,
			ActorID:    identity.MerchantUserID,
			Action:     "store_accept",
			FromStatus: stringPtr(row.Status),
			ToStatus:   stringPtr("accepted"),
			RequestID:  requestctx.RequestIDPtr(ctx),
		}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "store.order.accept", "order", orderID, row, map[string]string{"status": "accepted"}); err != nil {
			return err
		}
		eventID := uuid.NewString()
		payload := map[string]any{"order_id": idString(orderID), "shop_id": idString(row.ShopID), "order_no": row.OrderNo}
		if err := s.createOutboxWithID(ctx, tx, eventID, "store.order.accepted", "order", orderID, payload); err != nil {
			return err
		}
		if s.cp1.PrintEnabled {
			if err := printjob.EnqueueAuto(ctx, tx, s.idGen, row.ShopID, orderID, eventID, "order_accepted", payload); err != nil {
				return err
			}
		}
		row.Status = "accepted"
		items, err := s.repo.OrderItems(ctx, tx, orderID)
		if err != nil {
			return err
		}
		resp = storeOrderDTO(row, items)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	return resp, err
}

// StartPreparingOrder 将商户已接订单推进到备货中状态。
func (s *Service) StartPreparingOrder(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string) (StoreOrderDTO, error) {
	identity, orderID, err := s.merchantOrderActionInput(claims, orderIDRaw)
	if err != nil {
		return StoreOrderDTO{}, err
	}
	var resp StoreOrderDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, idempotency.RequestHash(map[string]string{"order_id": orderIDRaw, "action": "start_preparing"}))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}
		row, err := s.repo.LockAuthorizedOrder(ctx, tx, identity.MerchantID, identity.ShopIDs, orderID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ORDER_NOT_FOUND", "order not found")
		}
		if err != nil {
			return err
		}
		if row.Status != "accepted" {
			return problem.Conflict("ORDER_INVALID_STATUS", "only accepted orders can start preparing")
		}
		if err := s.repo.UpdateOrder(ctx, tx, orderID, map[string]any{"status": "preparing"}); err != nil {
			return err
		}
		if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{ID: s.idGen.Next(), OrderID: orderID, ActorType: claims.AccountType, ActorID: identity.MerchantUserID, Action: "store_start_preparing", FromStatus: stringPtr(row.Status), ToStatus: stringPtr("preparing"), RequestID: requestctx.RequestIDPtr(ctx)}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "store.order.start_preparing", "order", orderID, row, map[string]string{"status": "preparing"}); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "store.order.preparing", "order", orderID, map[string]any{"order_id": idString(orderID)}); err != nil {
			return err
		}
		row.Status = "preparing"
		items, err := s.repo.OrderItems(ctx, tx, orderID)
		if err != nil {
			return err
		}
		resp = storeOrderDTO(row, items)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	return resp, err
}

// PrepareOrder 只打开取货门禁。配送单和调度任务已在支付成功事务创建，
// 因此骑手可以在门店备货期间接受邀约或抢单。
func (s *Service) PrepareOrder(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string) (StoreOrderDTO, error) {
	identity, orderID, err := s.merchantOrderActionInput(claims, orderIDRaw)
	if err != nil {
		return StoreOrderDTO{}, err
	}
	reqHash := idempotency.RequestHash(map[string]string{"order_id": orderIDRaw, "action": "prepare"})

	var resp StoreOrderDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, reqHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}

		row, err := s.repo.LockAuthorizedOrder(ctx, tx, identity.MerchantID, identity.ShopIDs, orderID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ORDER_NOT_FOUND", "order not found")
		}
		if err != nil {
			return err
		}
		if row.Status != "accepted" && row.Status != "preparing" {
			return problem.Conflict("ORDER_INVALID_STATUS", "only accepted or preparing orders can be prepared")
		}
		shop, err := s.repo.LockAuthorizedShop(ctx, tx, identity.MerchantID, identity.ShopIDs, row.ShopID)
		if err != nil {
			return err
		}
		delivery, err := s.repo.LockDeliveryByOrder(ctx, tx, orderID)
		if errors.Is(err, gorm.ErrRecordNotFound) && s.dispatch != nil {
			created, _, createErr := s.dispatch.EnsurePaidOrderTask(ctx, tx, dispatch.PaidOrderInput{OrderID: orderID, ShopID: row.ShopID, AddressSnapshot: row.AddressSnapshot})
			err = createErr
			delivery = DeliveryOrder{
				ID:                created.ID,
				OrderID:           created.OrderID,
				ShopID:            created.ShopID,
				RiderID:           created.RiderID,
				Status:            created.Status,
				DispatchStatus:    created.DispatchStatus,
				PickupReadyStatus: created.PickupReadyStatus,
				PickupReadyAt:     created.PickupReadyAt,
				PickupSnapshot:    created.PickupSnapshot,
				RecipientSnapshot: created.RecipientSnapshot,
			}
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.Conflict("DELIVERY_NOT_CREATED", "paid order has no delivery task")
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if err := s.repo.MarkDeliveryPickupReady(ctx, tx, delivery.ID, jsonData(pickupSnapshot(shop)), row.AddressSnapshot, now); err != nil {
			return err
		}
		if s.cp1.PickupVerificationMode != "" || s.cp1.DeliveryVerificationMode != "" {
			if err := deliveryverification.GeneratePair(ctx, tx, s.cp1, s.idGen, delivery.ID); err != nil {
				return err
			}
		}
		deliveryStatus := "pending_assign"
		if delivery.RiderID != nil {
			deliveryStatus = "accepted"
		}
		if err := s.repo.UpdateOrder(ctx, tx, orderID, map[string]any{
			"status":          "ready_for_pickup",
			"delivery_status": deliveryStatus,
		}); err != nil {
			return err
		}
		if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{
			ID:         s.idGen.Next(),
			OrderID:    orderID,
			ActorType:  claims.AccountType,
			ActorID:    identity.MerchantUserID,
			Action:     "store_prepare",
			FromStatus: stringPtr(row.Status),
			ToStatus:   stringPtr("ready_for_pickup"),
			RequestID:  requestctx.RequestIDPtr(ctx),
		}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "store.order.prepare", "order", orderID, row, map[string]string{"status": "ready_for_pickup"}); err != nil {
			return err
		}
		eventID := uuid.NewString()
		payload := map[string]any{"order_id": idString(orderID), "shop_id": idString(row.ShopID), "order_no": row.OrderNo}
		if err := s.createOutboxWithID(ctx, tx, eventID, "store.order.prepared", "order", orderID, payload); err != nil {
			return err
		}
		if s.cp1.PrintEnabled {
			if err := printjob.EnqueueAuto(ctx, tx, s.idGen, row.ShopID, orderID, eventID, "order_prepared", payload); err != nil {
				return err
			}
		}
		row.Status = "ready_for_pickup"
		row.DeliveryStatus = deliveryStatus
		items, err := s.repo.OrderItems(ctx, tx, orderID)
		if err != nil {
			return err
		}
		resp = storeOrderDTO(row, items)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	return resp, err
}

// UpdateBusinessStatus 修改门店是否对外营业。
// 商品列表依赖 shops.business_status，因此这里需要审计并发出事件。
func (s *Service) UpdateBusinessStatus(ctx context.Context, claims *auth.Claims, method string, path string, key string, shopIDRaw string, req BusinessStatusReq) (map[string]string, error) {
	identity, err := merchantIdentityFromClaims(claims)
	if err != nil {
		return nil, err
	}
	shopID, err := parseID(shopIDRaw)
	if err != nil {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop id")
	}

	var resp map[string]string
	var productIDsForCache []uint64
	var serviceCityCode string
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}
		shop, err := s.repo.LockAuthorizedShop(ctx, tx, identity.MerchantID, identity.ShopIDs, shopID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("SHOP_NOT_FOUND", "shop not found")
		}
		if err != nil {
			return err
		}
		if err := s.repo.UpdateShop(ctx, tx, shopID, map[string]any{"business_status": req.BusinessStatus}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "shop.business_status.update", "shop", shopID, shop, map[string]string{"business_status": req.BusinessStatus}); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "shop.business_status.updated", "shop", shopID, map[string]any{"shop_id": idString(shopID), "business_status": req.BusinessStatus}); err != nil {
			return err
		}
		productIDs, err := s.repo.ProductIDsByShop(ctx, tx, shopID)
		if err != nil {
			return err
		}
		productIDsForCache = productIDs
		if shop.CityCode != nil {
			serviceCityCode = *shop.CityCode
		}
		resp = map[string]string{"shop_id": idString(shopID), "business_status": req.BusinessStatus}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	if err == nil {
		s.invalidateProductCachesForShop(ctx, shopID, productIDsForCache)
		if s.redis != nil && serviceCityCode != "" {
			_ = s.redis.Incr(ctx, "service_city_version:"+serviceCityCode).Err()
			_ = s.redis.Incr(ctx, "home_version:"+serviceCityCode).Err()
		}
	}
	return resp, err
}

// ListShopProducts 暴露商户范围内的上架商品和库存数量。
func (s *Service) ListShopProducts(ctx context.Context, claims *auth.Claims, query pagination.Query, shopIDRaw string) ([]ShopProductDTO, string, error) {
	identity, err := merchantIdentityFromClaims(claims)
	if err != nil {
		return nil, "", err
	}
	var shopID uint64
	if shopIDRaw != "" {
		shopID, err = parseID(shopIDRaw)
		if err != nil {
			return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid shop_id")
		}
		if !containsID(identity.ShopIDs, shopID) {
			return nil, "", problem.Forbidden("PERM_FORBIDDEN", "shop is not authorized")
		}
	}
	rows, err := s.repo.ListShopProducts(ctx, identity.MerchantID, identity.ShopIDs, shopID, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		nextPageToken = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]ShopProductDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, shopProductDTO(row))
	}
	return items, nextPageToken, nil
}

// CreateShopProduct 将平台商品加入授权门店，并初始化库存行。
func (s *Service) CreateShopProduct(ctx context.Context, claims *auth.Claims, method string, path string, key string, req ShopProductCreateReq) (ShopProductDTO, error) {
	identity, err := merchantIdentityFromClaims(claims)
	if err != nil {
		return ShopProductDTO{}, err
	}
	shopID, err := parseID(req.ShopID)
	if err != nil {
		return ShopProductDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop_id")
	}
	productID, err := parseID(req.ProductID)
	if err != nil {
		return ShopProductDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid product_id")
	}
	if !containsID(identity.ShopIDs, shopID) {
		return ShopProductDTO{}, problem.Forbidden("PERM_FORBIDDEN", "shop is not authorized")
	}
	status := req.Status
	if status == "" {
		status = "draft"
	}

	var resp ShopProductDTO
	var productIDForCache uint64
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}
		if _, err := s.repo.LockAuthorizedShop(ctx, tx, identity.MerchantID, identity.ShopIDs, shopID); errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("SHOP_NOT_FOUND", "shop not found")
		} else if err != nil {
			return err
		}
		product, err := s.repo.Product(ctx, tx, productID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("PRODUCT_NOT_FOUND", "product not found")
		}
		if err != nil {
			return err
		}
		shopProduct := ShopProduct{
			ID:              s.idGen.Next(),
			MerchantID:      identity.MerchantID,
			ShopID:          shopID,
			ProductID:       productID,
			SalePriceAmount: req.SalePriceAmount,
			Status:          status,
			SortOrder:       req.SortOrder,
		}
		if err := s.repo.CreateShopProduct(ctx, tx, shopProduct); isDuplicateKey(err) {
			return problem.Conflict("SHOP_PRODUCT_EXISTS", "shop product already exists")
		} else if err != nil {
			return err
		}
		stock := ProductStock{
			ID:            s.idGen.Next(),
			ShopProductID: shopProduct.ID,
			ShopID:        shopID,
			ProductID:     productID,
			AvailableQty:  req.InitialAvailableQty,
		}
		if err := s.repo.CreateStock(ctx, tx, stock); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "shop_product.create", "shop_product", shopProduct.ID, nil, shopProduct); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "shop_product.created", "shop_product", shopProduct.ID, map[string]any{"shop_product_id": idString(shopProduct.ID)}); err != nil {
			return err
		}
		if err := s.createCacheInvalidation(ctx, tx, productID, "shop_product_created"); err != nil {
			return err
		}
		productIDForCache = productID
		resp = shopProductDTO(ShopProductRow{
			ID:                  shopProduct.ID,
			MerchantID:          shopProduct.MerchantID,
			ShopID:              shopProduct.ShopID,
			ProductID:           shopProduct.ProductID,
			CategoryID:          product.CategoryID,
			Name:                product.Name,
			BrandName:           product.BrandName,
			Spec:                product.Spec,
			ImageURL:            product.ImageURL,
			SalePriceAmount:     shopProduct.SalePriceAmount,
			OriginalPriceAmount: product.OriginalPriceAmount,
			AgeRestricted:       product.AgeRestricted,
			Status:              shopProduct.Status,
			SortOrder:           shopProduct.SortOrder,
			AvailableQty:        stock.AvailableQty,
		})
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	if err == nil && productIDForCache != 0 {
		// 缓存删除发生在事务提交后；失败会记录为 cache.invalidate outbox 事件。
		s.invalidateProductCache(ctx, productIDForCache)
	}
	return resp, err
}

// UpdateShopProduct 只修改商户可控的门店售卖字段，不修改平台商品目录字段。
func (s *Service) UpdateShopProduct(ctx context.Context, claims *auth.Claims, method string, path string, key string, shopProductIDRaw string, req ShopProductUpdateReq) (map[string]string, error) {
	identity, shopProductID, err := s.merchantShopProductActionInput(claims, shopProductIDRaw)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if req.SalePriceAmount != nil {
		values["sale_price_amount"] = *req.SalePriceAmount
	}
	if req.Status != nil {
		values["status"] = *req.Status
	}
	if req.SortOrder != nil {
		values["sort_order"] = *req.SortOrder
	}
	if len(values) == 0 {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "no fields to update")
	}

	var resp map[string]string
	var productIDForCache uint64
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}
		row, err := s.repo.LockAuthorizedShopProduct(ctx, tx, identity.MerchantID, identity.ShopIDs, shopProductID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("SHOP_PRODUCT_NOT_FOUND", "shop product not found")
		}
		if err != nil {
			return err
		}
		if err := s.repo.UpdateShopProduct(ctx, tx, shopProductID, values); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "shop_product.update", "shop_product", shopProductID, row, values); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "shop_product.updated", "shop_product", shopProductID, map[string]any{"shop_product_id": idString(shopProductID)}); err != nil {
			return err
		}
		if err := s.createCacheInvalidation(ctx, tx, row.ProductID, "shop_product_updated"); err != nil {
			return err
		}
		productIDForCache = row.ProductID
		resp = map[string]string{"shop_product_id": idString(shopProductID)}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	if err == nil && productIDForCache != 0 {
		s.invalidateProductCache(ctx, productIDForCache)
	}
	return resp, err
}

// AdjustStock 修改一个授权门店商品的可售库存。
// 这里不修改预占库存，因为预占库存由订单/支付流程管理。
func (s *Service) AdjustStock(ctx context.Context, claims *auth.Claims, method string, path string, key string, shopProductIDRaw string, req StockAdjustReq) (ShopProductDTO, error) {
	if req.QuantityDelta == 0 {
		return ShopProductDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "quantity_delta cannot be zero")
	}
	identity, shopProductID, err := s.merchantShopProductActionInput(claims, shopProductIDRaw)
	if err != nil {
		return ShopProductDTO{}, err
	}

	var resp ShopProductDTO
	var productIDForCache uint64
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}
		shopProduct, err := s.repo.LockAuthorizedShopProduct(ctx, tx, identity.MerchantID, identity.ShopIDs, shopProductID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("SHOP_PRODUCT_NOT_FOUND", "shop product not found")
		}
		if err != nil {
			return err
		}
		// 计算前先锁定库存行，避免并发调整把库存扣成负数。
		stock, err := s.repo.LockStock(ctx, tx, shopProductID)
		if err != nil {
			return err
		}
		afterAvailable := stock.AvailableQty + req.QuantityDelta
		if afterAvailable < 0 {
			return problem.Conflict("STOCK_NOT_ENOUGH", "available stock cannot be negative")
		}
		if err := s.repo.AdjustStock(ctx, tx, stock.ID, req.QuantityDelta); err != nil {
			return err
		}
		if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{
			ID:                 s.idGen.Next(),
			ShopProductID:      shopProduct.ID,
			ShopID:             shopProduct.ShopID,
			ProductID:          shopProduct.ProductID,
			ChangeType:         "adjust",
			QuantityDelta:      req.QuantityDelta,
			BeforeAvailableQty: stock.AvailableQty,
			AfterAvailableQty:  afterAvailable,
			SourceType:         "merchant_adjust",
			SourceID:           shopProduct.ID,
			IdempotencyKey:     stringPtr(key),
		}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "stock.adjust", "shop_product", shopProductID, stock, map[string]any{"available_qty": afterAvailable, "reason": req.Reason}); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "stock.adjusted", "shop_product", shopProductID, map[string]any{"shop_product_id": idString(shopProductID), "quantity_delta": req.QuantityDelta}); err != nil {
			return err
		}
		if err := s.createCacheInvalidation(ctx, tx, shopProduct.ProductID, "stock_adjusted"); err != nil {
			return err
		}
		product, err := s.repo.Product(ctx, tx, shopProduct.ProductID)
		if err != nil {
			return err
		}
		productIDForCache = shopProduct.ProductID
		resp = shopProductDTO(ShopProductRow{
			ID:                  shopProduct.ID,
			MerchantID:          shopProduct.MerchantID,
			ShopID:              shopProduct.ShopID,
			ProductID:           shopProduct.ProductID,
			CategoryID:          product.CategoryID,
			Name:                product.Name,
			BrandName:           product.BrandName,
			Spec:                product.Spec,
			ImageURL:            product.ImageURL,
			SalePriceAmount:     shopProduct.SalePriceAmount,
			OriginalPriceAmount: product.OriginalPriceAmount,
			Status:              shopProduct.Status,
			SortOrder:           shopProduct.SortOrder,
			AvailableQty:        afterAvailable,
			ReservedQty:         stock.ReservedQty,
			LockedQty:           stock.LockedQty,
		})
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	if err == nil && productIDForCache != 0 {
		s.invalidateProductCache(ctx, productIDForCache)
	}
	return resp, err
}

// merchantOrderActionInput 返回商户订单操作输入。
func (s *Service) merchantOrderActionInput(claims *auth.Claims, orderIDRaw string) (merchantIdentity, uint64, error) {
	identity, err := merchantIdentityFromClaims(claims)
	if err != nil {
		return merchantIdentity{}, 0, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return merchantIdentity{}, 0, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}
	return identity, orderID, nil
}

// merchantShopProductActionInput 返回商户门店商品操作输入。
func (s *Service) merchantShopProductActionInput(claims *auth.Claims, shopProductIDRaw string) (merchantIdentity, uint64, error) {
	identity, err := merchantIdentityFromClaims(claims)
	if err != nil {
		return merchantIdentity{}, 0, err
	}
	shopProductID, err := parseID(shopProductIDRaw)
	if err != nil {
		return merchantIdentity{}, 0, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop product id")
	}
	return identity, shopProductID, nil
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

// createOutbox 创建发件箱事件。
func (s *Service) createOutbox(ctx context.Context, tx *gorm.DB, eventType string, aggregateType string, aggregateID uint64, payload any) error {
	return s.createOutboxWithID(ctx, tx, uuid.NewString(), eventType, aggregateType, aggregateID, payload)
}

// createOutboxWithID 创建发件箱事件 With ID。
func (s *Service) createOutboxWithID(ctx context.Context, tx *gorm.DB, eventID string, eventType string, aggregateType string, aggregateID uint64, payload any) error {
	return s.repo.CreateOutbox(ctx, tx, OutboxEvent{
		ID:            s.idGen.Next(),
		EventID:       eventID,
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
	return s.createOutbox(ctx, tx, "cache.invalidate", "product", productID, map[string]any{"patterns": []string{s.productCachePattern(productID)}, "reason": reason})
}

// createAudit 记录商户侧写操作，便于后台复核和纠纷追踪。
func (s *Service) createAudit(ctx context.Context, tx *gorm.DB, actorID uint64, action string, resourceType string, resourceID uint64, before any, after any) error {
	return s.repo.CreateAuditLog(ctx, tx, AuditLog{
		ID:           s.idGen.Next(),
		ActorType:    "merchant",
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		BeforeData:   jsonData(before),
		AfterData:    jsonData(after),
		Result:       "success",
		RequestID:    requestctx.RequestIDPtr(ctx),
		IP:           requestctx.IPPtr(ctx),
		UserAgent:    requestctx.UserAgentPtr(ctx),
	})
}

// invalidateProductCache 遵循 cache-aside：先提交 MySQL，再删除 Redis 缓存。
// 可靠 cache.invalidate 已在业务事务内写入；这里的同步删除只用于降低读旧值窗口。
func (s *Service) invalidateProductCache(ctx context.Context, productID uint64) {
	if s.redis == nil || productID == 0 {
		return
	}
	keys, err := s.productCacheKeys(ctx, productID)
	if err == nil && len(keys) > 0 {
		err = s.redis.Del(ctx, keys...).Err()
	}
	_ = s.redis.Incr(ctx, "home_version:global").Err()
}

// productCacheKeys 返回商品缓存 Keys。
func (s *Service) productCacheKeys(ctx context.Context, productID uint64) ([]string, error) {
	pattern := s.productCachePattern(productID)
	var cursor uint64
	var keys []string
	for {
		batch, next, err := s.redis.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return keys, nil
}

// productCachePattern 返回商品缓存 Pattern。
func (s *Service) productCachePattern(productID uint64) string {
	return "product:detail:" + idString(productID) + "*"
}

// invalidateProductCachesForShop 使商品 Caches For 门店失效。
func (s *Service) invalidateProductCachesForShop(ctx context.Context, shopID uint64, productIDs []uint64) {
	if s.redis == nil || len(productIDs) == 0 {
		return
	}
	keys := make([]string, 0, len(productIDs))
	seen := map[uint64]struct{}{}
	for _, productID := range productIDs {
		if productID == 0 {
			continue
		}
		if _, ok := seen[productID]; ok {
			continue
		}
		seen[productID] = struct{}{}
		matched, err := s.productCacheKeys(ctx, productID)
		if err != nil {
			return
		}
		keys = append(keys, matched...)
	}
	if len(keys) == 0 {
		return
	}
	_ = s.redis.Del(ctx, keys...).Err()
}

type merchantIdentity struct {
	MerchantUserID uint64
	MerchantID     uint64
	ShopIDs        []uint64
}

// merchantIdentityFromClaims 是登录 claims 和商户对象范围之间的边界。
// 所有 store 操作在接触门店/订单/商品行之前都要先经过这里。
func merchantIdentityFromClaims(claims *auth.Claims) (merchantIdentity, error) {
	if claims == nil || claims.AccountType != "merchant" {
		return merchantIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "merchant account required")
	}
	merchantUserID, err := parseID(claims.MerchantUserID)
	if err != nil {
		return merchantIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "invalid merchant identity")
	}
	merchantID, err := parseID(claims.MerchantID)
	if err != nil {
		return merchantIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "invalid merchant identity")
	}
	shopIDs := make([]uint64, 0, len(claims.AuthorizedShopIDs))
	for _, raw := range claims.AuthorizedShopIDs {
		shopID, err := parseID(raw)
		if err != nil {
			return merchantIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "invalid shop authorization")
		}
		shopIDs = append(shopIDs, shopID)
	}
	if len(shopIDs) == 0 {
		return merchantIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "no authorized shops")
	}
	return merchantIdentity{MerchantUserID: merchantUserID, MerchantID: merchantID, ShopIDs: shopIDs}, nil
}

// storeOrderDTO 返回门店订单DTO。
func storeOrderDTO(row Order, items []OrderItem) StoreOrderDTO {
	dto := StoreOrderDTO{
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
		BrandName:       snapshot["brand_name"],
		Spec:            snapshot["spec"],
		Quantity:        row.Quantity,
		SalePriceAmount: row.SalePriceAmount,
		TotalAmount:     row.TotalAmount,
	}
}

// shopProductDTO 返回门店商品DTO。
func shopProductDTO(row ShopProductRow) ShopProductDTO {
	return ShopProductDTO{
		ID:                  idString(row.ID),
		MerchantID:          idString(row.MerchantID),
		ShopID:              idString(row.ShopID),
		ProductID:           idString(row.ProductID),
		CategoryID:          idString(row.CategoryID),
		Name:                row.Name,
		BrandName:           stringValue(row.BrandName),
		Spec:                stringValue(row.Spec),
		ImageURL:            stringValue(row.ImageURL),
		SalePriceAmount:     row.SalePriceAmount,
		OriginalPriceAmount: row.OriginalPriceAmount,
		Status:              row.Status,
		SortOrder:           row.SortOrder,
		AvailableQty:        row.AvailableQty,
		ReservedQty:         row.ReservedQty,
		LockedQty:           row.LockedQty,
		AgeRestricted:       row.AgeRestricted,
	}
}

// pickupSnapshot 返回pickup 快照。
func pickupSnapshot(shop Shop) map[string]any {
	return map[string]any{
		"shop_id":           idString(shop.ID),
		"name":              shop.Name,
		"phone":             stringValue(shop.Phone),
		"province":          stringValue(shop.Province),
		"city":              shop.City,
		"district":          shop.District,
		"address":           shop.Address,
		"latitude":          shop.Latitude,
		"longitude":         shop.Longitude,
		"coordinate_system": shop.CoordinateSystem,
	}
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, problem.InvalidArgument("VALIDATION_FAILED", "invalid id")
	}
	return id, nil
}

// containsID 判断contains ID。
func containsID(values []uint64, expected uint64) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// idString 将数字 ID 转换为字符串。
func idString(id uint64) string {
	return strconv.FormatUint(id, 10)
}

// stringPtr 将非空字符串转换为字符串指针。
func stringPtr(value string) *string {
	return &value
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

// isDuplicateKey 判断重复项密钥是否成立。
func isDuplicateKey(err error) bool {
	var mysqlErr *mysqlerr.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}
