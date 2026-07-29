package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
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
	repo                   *Repository
	redis                  *goredis.Client
	idStore                *idempotency.Store
	idGen                  *snowflake.Generator
	cp1                    config.CP1Config
	dispatch               *dispatch.Service
	generatePickupVerifier func(context.Context, *gorm.DB, config.CP1Config, *snowflake.Generator, uint64) error
}

// WithCP1 设置CP 1并返回更新后的值。
func (s *Service) WithCP1(cfg config.CP1Config) *Service { s.cp1 = cfg; return s }

// WithDispatch 设置调度并返回更新后的值。
func (s *Service) WithDispatch(service *dispatch.Service) *Service { s.dispatch = service; return s }

// NewService 负责商户侧订单履约、门店商品和库存操作。
func NewService(db *gorm.DB, redisClient *goredis.Client, idGen *snowflake.Generator) *Service {
	return &Service{
		repo:                   NewRepository(db),
		redis:                  redisClient,
		idStore:                idempotency.NewStore(db),
		idGen:                  idGen,
		generatePickupVerifier: deliveryverification.GeneratePair,
	}
}

// ListOrders 只返回当前商户账号授权门店下的订单。
func (s *Service) ListOrders(ctx context.Context, claims *auth.Claims, query pagination.Query, filters StoreOrderListFilters) ([]StoreOrderSummaryDTO, string, error) {
	identity, err := merchantIdentityWithPermission(claims, "store_order:list")
	if err != nil {
		return nil, "", err
	}
	if filters.ShopID != 0 && !containsID(identity.ShopIDs, filters.ShopID) {
		return nil, "", problem.Forbidden("SHOP_SCOPE_FORBIDDEN", "shop is not authorized")
	}
	rows, err := s.repo.ListOrders(ctx, identity.MerchantID, identity.ShopIDs, filters, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		if storeOrderUsesKeyset(query.OrderBy) {
			last := rows[len(rows)-1]
			nextPageToken = pagination.NextPageTokenWithCursor(query, last.CreatedAt.Format(time.RFC3339Nano), strconv.FormatUint(last.ID, 10))
		} else {
			nextPageToken = pagination.NextPageToken(query)
		}
	}
	orderIDs := make([]uint64, 0, len(rows))
	shopIDs := make([]uint64, 0, len(rows))
	for _, row := range rows {
		orderIDs = append(orderIDs, row.ID)
		shopIDs = append(shopIDs, row.ShopID)
	}
	orderItems, err := s.repo.OrderItemsForOrders(ctx, orderIDs)
	if err != nil {
		return nil, "", err
	}
	shops, err := s.repo.AuthorizedShops(ctx, identity.MerchantID, shopIDs)
	if err != nil {
		return nil, "", err
	}
	itemsByOrder := make(map[uint64][]OrderItem, len(rows))
	for _, item := range orderItems {
		itemsByOrder[item.OrderID] = append(itemsByOrder[item.OrderID], item)
	}
	shopsByID := make(map[uint64]Shop, len(shops))
	for _, shop := range shops {
		shopsByID[shop.ID] = shop
	}
	items := make([]StoreOrderSummaryDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, storeOrderSummaryDTO(row, shopsByID[row.ShopID], itemsByOrder[row.ID]))
	}
	return items, nextPageToken, nil
}

// DetailOrder 返回商家专用订单详情。功能权限在读取订单前检查，对象范围
// 直接进入订单查询条件，范围外与不存在订单统一返回 404。
func (s *Service) DetailOrder(ctx context.Context, claims *auth.Claims, orderIDRaw string) (StoreOrderDetailDTO, error) {
	identity, err := merchantIdentityWithPermission(claims, "store_order:view")
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return StoreOrderDetailDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}

	var result StoreOrderDetailDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var loadErr error
		result, loadErr = s.loadStoreOrderDetail(ctx, tx, identity, orderID)
		return loadErr
	})
	return result, err
}

// loadStoreOrderDetail 从当前事务重新加载完整且对商户安全的投影。
// 每次修改后的操作响应都使用它，确保返回的状态、版本、日志和配送事实
// 均为操作后的最新事实。
func (s *Service) loadStoreOrderDetail(ctx context.Context, db *gorm.DB, identity merchantIdentity, orderID uint64) (StoreOrderDetailDTO, error) {
	row, err := s.repo.AuthorizedOrder(ctx, db, identity.MerchantID, identity.ShopIDs, orderID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return StoreOrderDetailDTO{}, problem.NotFound("ORDER_NOT_FOUND", "order not found")
	}
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	shop, err := s.repo.OrderShop(ctx, db, identity.MerchantID, row.ShopID)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	items, err := s.repo.OrderItems(ctx, db, orderID)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	payment, paymentErr := s.repo.PaymentByOrder(ctx, db, orderID)
	if paymentErr != nil && !errors.Is(paymentErr, gorm.ErrRecordNotFound) {
		return StoreOrderDetailDTO{}, paymentErr
	}
	delivery, deliveryErr := s.repo.DeliveryByOrder(ctx, db, orderID)
	if deliveryErr != nil && !errors.Is(deliveryErr, gorm.ErrRecordNotFound) {
		return StoreOrderDetailDTO{}, deliveryErr
	}
	logs, err := s.repo.RecentOrderLogs(ctx, db, orderID, 20)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	return storeOrderDetailDTO(row, shop, items, payment, paymentErr == nil, delivery, deliveryErr == nil, logs), nil
}

// AcceptOrder 将已支付订单推进到商户已接单状态。
// 带授权范围的订单锁可防止商户接到其他商户的订单。
func (s *Service) AcceptOrder(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string, req StoreOrderActionReq) (resp StoreOrderDetailDTO, resultErr error) {
	var failureVersion *int
	defer func() {
		s.auditStoreOrderActionFailure(ctx, claims, method, path, "store.order.accept", orderIDRaw, failureVersion, resultErr)
	}()
	identity, orderID, err := s.merchantOrderActionInput(claims, orderIDRaw, "store_order:accept")
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	expectedVersion, err := storeOrderExpectedVersion(req)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	reqHash := storeOrderActionRequestHash(orderID, "accept", req)
	deliveryID, err := s.deliveryActionProbe(ctx, orderID)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}

	resultErr = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, reqHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}

		_, row, err := s.lockDeliveryThenAuthorizedOrder(
			ctx, tx, identity, orderID, deliveryID,
		)
		if err != nil {
			return err
		}
		observedVersion := row.Version
		failureVersion = &observedVersion
		if row.Version < 0 || uint(row.Version) != expectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "order version changed")
		}
		if row.Status != "paid" {
			return problem.Conflict("ORDER_INVALID_STATUS", "only paid orders can be accepted")
		}
		updated, err := s.repo.TransitionOrder(ctx, tx, orderID, row.Status, row.Version, map[string]any{"status": "accepted"})
		if err != nil {
			return err
		}
		if !updated {
			return problem.Conflict("VERSION_CONFLICT", "order changed concurrently")
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
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "store.order.accept", "order", orderID, row, map[string]any{"status": "accepted", "version": row.Version + 1}); err != nil {
			return err
		}
		eventID := uuid.NewString()
		payload := map[string]any{"order_id": idString(orderID), "shop_id": idString(row.ShopID), "order_no": row.OrderNo, "version": row.Version + 1}
		if err := s.createOutboxWithID(ctx, tx, eventID, "store.order.accepted", "order", orderID, payload); err != nil {
			return err
		}
		if s.cp1.PrintEnabled {
			if err := printjob.EnqueueAuto(ctx, tx, s.idGen, row.ShopID, orderID, eventID, "order_accepted", payload); err != nil {
				return err
			}
		}
		resp, err = s.loadStoreOrderDetail(ctx, tx, identity, orderID)
		if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	return resp, resultErr
}

// StartPreparingOrder 将商户已接订单推进到备货中状态。
func (s *Service) StartPreparingOrder(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string, req StoreOrderActionReq) (resp StoreOrderDetailDTO, resultErr error) {
	var failureVersion *int
	defer func() {
		s.auditStoreOrderActionFailure(ctx, claims, method, path, "store.order.start_preparing", orderIDRaw, failureVersion, resultErr)
	}()
	identity, orderID, err := s.merchantOrderActionInput(claims, orderIDRaw, "store_order:prepare")
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	expectedVersion, err := storeOrderExpectedVersion(req)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	deliveryID, err := s.deliveryActionProbe(ctx, orderID)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	resultErr = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, storeOrderActionRequestHash(orderID, "start_preparing", req))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}
		_, row, err := s.lockDeliveryThenAuthorizedOrder(
			ctx, tx, identity, orderID, deliveryID,
		)
		if err != nil {
			return err
		}
		observedVersion := row.Version
		failureVersion = &observedVersion
		if row.Version < 0 || uint(row.Version) != expectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "order version changed")
		}
		if row.Status != "accepted" {
			return problem.Conflict("ORDER_INVALID_STATUS", "only accepted orders can start preparing")
		}
		updated, err := s.repo.TransitionOrder(ctx, tx, orderID, row.Status, row.Version, map[string]any{"status": "preparing"})
		if err != nil {
			return err
		}
		if !updated {
			return problem.Conflict("VERSION_CONFLICT", "order changed concurrently")
		}
		if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{ID: s.idGen.Next(), OrderID: orderID, ActorType: claims.AccountType, ActorID: identity.MerchantUserID, Action: "store_start_preparing", FromStatus: stringPtr(row.Status), ToStatus: stringPtr("preparing"), RequestID: requestctx.RequestIDPtr(ctx)}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "store.order.start_preparing", "order", orderID, row, map[string]any{"status": "preparing", "version": row.Version + 1}); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "store.order.preparing", "order", orderID, map[string]any{"order_id": idString(orderID), "shop_id": idString(row.ShopID), "order_no": row.OrderNo, "version": row.Version + 1}); err != nil {
			return err
		}
		resp, err = s.loadStoreOrderDetail(ctx, tx, identity, orderID)
		if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	return resp, resultErr
}

// PrepareOrder 只打开取货门禁。配送单和调度任务已在支付成功事务创建，
// 因此骑手可以在门店备货期间接受邀约或抢单。
func (s *Service) PrepareOrder(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string, req StoreOrderActionReq) (resp StoreOrderDetailDTO, resultErr error) {
	var failureVersion *int
	defer func() {
		s.auditStoreOrderActionFailure(ctx, claims, method, path, "store.order.prepare", orderIDRaw, failureVersion, resultErr)
	}()
	identity, orderID, err := s.merchantOrderActionInput(claims, orderIDRaw, "store_order:prepare")
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	expectedVersion, err := storeOrderExpectedVersion(req)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}
	reqHash := storeOrderActionRequestHash(orderID, "prepare", req)
	deliveryID, err := s.deliveryActionProbe(ctx, orderID)
	if err != nil {
		return StoreOrderDetailDTO{}, err
	}

	resultErr = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, reqHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, &resp)
		}

		delivery, row, err := s.lockDeliveryThenAuthorizedOrder(
			ctx, tx, identity, orderID, deliveryID,
		)
		if err != nil {
			return err
		}
		observedVersion := row.Version
		failureVersion = &observedVersion
		if row.Version < 0 || uint(row.Version) != expectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "order version changed")
		}
		if row.Status != "preparing" {
			return problem.Conflict("ORDER_INVALID_STATUS", "only preparing orders can be marked ready")
		}
		shop, err := s.repo.LockAuthorizedShop(ctx, tx, identity.MerchantID, identity.ShopIDs, row.ShopID)
		if err != nil {
			return err
		}
		now := time.Now()
		if err := s.repo.MarkDeliveryPickupReady(ctx, tx, delivery.ID, jsonData(pickupSnapshot(shop)), row.AddressSnapshot, now); err != nil {
			return err
		}
		if s.cp1.PickupVerificationMode != "" || s.cp1.DeliveryVerificationMode != "" {
			generate := s.generatePickupVerifier
			if generate == nil {
				generate = deliveryverification.GeneratePair
			}
			if err := generate(ctx, tx, s.cp1, s.idGen, delivery.ID); err != nil {
				return err
			}
		}
		deliveryStatus := "pending_assign"
		if delivery.RiderID != nil {
			deliveryStatus = "accepted"
		}
		updated, err := s.repo.TransitionOrder(ctx, tx, orderID, row.Status, row.Version, map[string]any{
			"status":          "ready_for_pickup",
			"delivery_status": deliveryStatus,
		})
		if err != nil {
			return err
		}
		if !updated {
			return problem.Conflict("VERSION_CONFLICT", "order changed concurrently")
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
		if err := s.createAudit(ctx, tx, identity.MerchantUserID, "store.order.prepare", "order", orderID, row, map[string]any{"status": "ready_for_pickup", "delivery_status": deliveryStatus, "version": row.Version + 1}); err != nil {
			return err
		}
		eventID := uuid.NewString()
		payload := map[string]any{"order_id": idString(orderID), "shop_id": idString(row.ShopID), "order_no": row.OrderNo, "version": row.Version + 1}
		if err := s.createOutboxWithID(ctx, tx, eventID, "store.order.prepared", "order", orderID, payload); err != nil {
			return err
		}
		if s.cp1.PrintEnabled {
			if err := printjob.EnqueueAuto(ctx, tx, s.idGen, row.ShopID, orderID, eventID, "order_prepared", payload); err != nil {
				return err
			}
		}
		resp, err = s.loadStoreOrderDetail(ctx, tx, identity, orderID)
		if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	return resp, resultErr
}

// UpdateBusinessStatus 修改门店是否对外营业。
// 商品列表依赖 shops.business_status，因此这里需要审计并发出事件。
func (s *Service) UpdateBusinessStatus(ctx context.Context, claims *auth.Claims, method string, path string, key string, shopIDRaw string, req BusinessStatusReq) (map[string]string, error) {
	identity, err := merchantIdentityWithPermission(claims, "shop:business_status")
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
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, idempotency.ResourceRequestHash("shop.business_status.update", shopID, req))
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
func (s *Service) ListShopProducts(ctx context.Context, claims *auth.Claims, query pagination.Query, filters StoreInventoryFilters) ([]ShopProductDTO, string, error) {
	identity, err := merchantIdentityWithPermission(claims, "inventory:view")
	if err != nil {
		return nil, "", err
	}
	if filters.ShopID != 0 && !containsID(identity.ShopIDs, filters.ShopID) {
		return nil, "", problem.Forbidden("SHOP_SCOPE_FORBIDDEN", "shop is not authorized")
	}
	rows, err := s.repo.ListShopProducts(ctx, identity.MerchantID, identity.ShopIDs, filters, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		if storeInventoryUsesKeyset(query.OrderBy) {
			last := rows[len(rows)-1]
			nextPageToken = pagination.NextPageTokenWithCursor(query, last.UpdatedAt.Format(time.RFC3339Nano), strconv.FormatUint(last.ID, 10))
		} else {
			nextPageToken = pagination.NextPageToken(query)
		}
	}
	items := make([]ShopProductDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, shopProductDTO(row))
	}
	return items, nextPageToken, nil
}

// CreateShopProduct 将平台商品加入授权门店，并初始化库存行。
func (s *Service) CreateShopProduct(ctx context.Context, claims *auth.Claims, method string, path string, key string, req ShopProductCreateReq) (ShopProductDTO, error) {
	identity, err := merchantIdentityWithPermission(claims, "shop_product:create")
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
			LowStockThreshold:   stock.LowStockThreshold,
			Version:             stock.Version,
			UpdatedAt:           time.Now().UTC(),
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
	identity, shopProductID, err := s.merchantShopProductActionInput(claims, shopProductIDRaw, "shop_product:update")
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
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, idempotency.ResourceRequestHash("shop_product.update", shopProductID, req))
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
	identity, shopProductID, err := s.merchantShopProductActionInput(claims, shopProductIDRaw, "inventory:adjust")
	if err != nil {
		return ShopProductDTO{}, err
	}

	var resp ShopProductDTO
	var productIDForCache uint64
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, identity.MerchantUserID, method, path, key, idempotency.ResourceRequestHash("shop_product.stock.adjust", shopProductID, req))
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
		beforeTotal := stock.AvailableQty + stock.ReservedQty + stock.LockedQty
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
			TotalQuantityDelta: req.QuantityDelta,
			BeforeTotalQty:     beforeTotal,
			AfterTotalQty:      beforeTotal + req.QuantityDelta,
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
			LowStockThreshold:   stock.LowStockThreshold,
			Version:             stock.Version + 1,
			UpdatedAt:           time.Now().UTC(),
		})
		return s.idStore.Succeed(ctx, tx, claims.AccountType, identity.MerchantUserID, path, key, resp)
	})
	if err == nil && productIDForCache != 0 {
		s.invalidateProductCache(ctx, productIDForCache)
	}
	return resp, err
}

// merchantOrderActionInput 返回商户订单操作输入。
func (s *Service) merchantOrderActionInput(claims *auth.Claims, orderIDRaw, permission string) (merchantIdentity, uint64, error) {
	identity, err := merchantIdentityWithPermission(claims, permission)
	if err != nil {
		return merchantIdentity{}, 0, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return merchantIdentity{}, 0, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}
	return identity, orderID, nil
}

func storeOrderExpectedVersion(req StoreOrderActionReq) (uint, error) {
	if req.ExpectedVersion == nil {
		return 0, problem.InvalidArgument("VALIDATION_FAILED", "expected_version is required")
	}
	return *req.ExpectedVersion, nil
}

func storeOrderActionRequestHash(orderID uint64, action string, req StoreOrderActionReq) string {
	return idempotency.RequestHash(struct {
		OrderID         string `json:"order_id"`
		Action          string `json:"action"`
		ExpectedVersion *uint  `json:"expected_version"`
	}{
		OrderID: idString(orderID), Action: action, ExpectedVersion: req.ExpectedVersion,
	})
}

// deliveryActionProbe 只执行无锁查询。
// 在 lockDeliveryThenAuthorizedOrder 重新读取关系前，返回的 ID 不可信。
func (s *Service) deliveryActionProbe(
	ctx context.Context,
	orderID uint64,
) (uint64, error) {
	deliveryID, err := s.repo.DeliveryIDByOrder(
		ctx,
		s.repo.DB(),
		orderID,
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return deliveryID, err
}

// lockDeliveryThenAuthorizedOrder 是商户订单操作唯一的锁前缀：
// delivery_order -> order。
// 配送记录缺失时使用无锁范围查询，使未授权订单与不存在订单保持不可区分，
// 同时避免重新引入旧的订单优先循环锁。
func (s *Service) lockDeliveryThenAuthorizedOrder(
	ctx context.Context,
	tx *gorm.DB,
	identity merchantIdentity,
	orderID uint64,
	deliveryID uint64,
) (DeliveryOrder, Order, error) {
	if deliveryID == 0 {
		_, err := s.repo.AuthorizedOrder(
			ctx,
			tx,
			identity.MerchantID,
			identity.ShopIDs,
			orderID,
		)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return DeliveryOrder{}, Order{}, problem.NotFound(
				"ORDER_NOT_FOUND",
				"order not found",
			)
		}
		if err != nil {
			return DeliveryOrder{}, Order{}, err
		}
		return DeliveryOrder{}, Order{}, problem.Conflict(
			"DELIVERY_NOT_CREATED",
			"paid order has no delivery task",
		)
	}
	deliveryRow, err := s.repo.LockDeliveryByID(ctx, tx, deliveryID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DeliveryOrder{}, Order{}, problem.Conflict(
			"DELIVERY_NOT_CREATED",
			"paid order has no delivery task",
		)
	}
	if err != nil {
		return DeliveryOrder{}, Order{}, err
	}
	if deliveryRow.OrderID != orderID {
		return DeliveryOrder{}, Order{}, problem.Conflict(
			"DELIVERY_RELATION_CHANGED",
			"delivery relation changed; refresh and retry",
		)
	}
	orderRow, err := s.repo.LockAuthorizedOrder(
		ctx,
		tx,
		identity.MerchantID,
		identity.ShopIDs,
		orderID,
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DeliveryOrder{}, Order{}, problem.NotFound(
			"ORDER_NOT_FOUND",
			"order not found",
		)
	}
	if err != nil {
		return DeliveryOrder{}, Order{}, err
	}
	if deliveryRow.ShopID != orderRow.ShopID {
		return DeliveryOrder{}, Order{}, problem.Conflict(
			"DELIVERY_RELATION_CHANGED",
			"delivery relation changed; refresh and retry",
		)
	}
	return deliveryRow, orderRow, nil
}

// merchantShopProductActionInput 返回商户门店商品操作输入。
func (s *Service) merchantShopProductActionInput(claims *auth.Claims, shopProductIDRaw, permission string) (merchantIdentity, uint64, error) {
	identity, err := merchantIdentityWithPermission(claims, permission)
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

// createOutboxWithID 使用指定 ID 创建发件箱事件。
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
		IPHash:       requestctx.IPHashPtr(ctx),
		UserAgent:    requestctx.UserAgentPtr(ctx),
	})
}

// auditStoreOrderActionFailure 在已回滚业务事务之外持久化被拒绝的商户操作。
// 它刻意只记录粗粒度请求元数据和稳定错误码；请求体、订单快照和客户数据
// 绝不会进入失败审计。
func (s *Service) auditStoreOrderActionFailure(ctx context.Context, claims *auth.Claims, method, path, action, orderIDRaw string, observedVersion *int, requestErr error) {
	if requestErr == nil || s == nil || s.repo == nil || s.repo.DB() == nil || s.idGen == nil {
		return
	}
	actorType, actorID := "unknown", uint64(0)
	if claims != nil {
		actorType = claims.AccountType
		switch claims.AccountType {
		case "merchant":
			actorID, _ = parseID(claims.MerchantUserID)
		case "admin":
			actorID, _ = parseID(claims.AdminUserID)
		case "rider":
			actorID, _ = parseID(claims.RiderID)
		case "customer":
			actorID, _ = parseID(claims.CustomerID)
		}
	}
	orderID, _ := parseID(orderIDRaw)
	details := problem.FromError(requestErr)
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	safe := map[string]string{
		"route":      strings.TrimSpace(method + " " + path),
		"error_code": details.ErrorCode,
	}
	if observedVersion != nil {
		safe["order_version"] = strconv.Itoa(*observedVersion)
	}
	_ = s.repo.CreateAuditLog(auditCtx, s.repo.DB(), AuditLog{
		ID:           s.idGen.Next(),
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       action,
		ResourceType: "order",
		ResourceID:   orderID,
		AfterData:    jsonData(safe),
		Result:       "failed",
		RequestID:    requestctx.RequestIDPtr(auditCtx),
		IPHash:       requestctx.IPHashPtr(auditCtx),
		UserAgent:    requestctx.UserAgentPtr(auditCtx),
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

func merchantIdentityWithPermission(claims *auth.Claims, permission string) (merchantIdentity, error) {
	identity, err := merchantIdentityFromClaims(claims)
	if err != nil {
		return merchantIdentity{}, err
	}
	for _, granted := range claims.Permissions {
		if granted == permission {
			return identity, nil
		}
	}
	return merchantIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "merchant permission required")
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

func storeOrderSummaryDTO(row Order, shop Shop, rows []OrderItem) StoreOrderSummaryDTO {
	var itemSummary StoreOrderItemSummaryDTO
	totalQuantity := 0
	for index, row := range rows {
		item := orderItemDTO(row)
		totalQuantity += item.Quantity
		if index == 0 {
			itemSummary = StoreOrderItemSummaryDTO{
				ProductID: item.ProductID,
				Name:      item.Name,
				Spec:      item.Spec,
				ImageURL:  item.ImageURL,
				Quantity:  item.Quantity,
			}
		}
	}
	address, contactMask := storedAddressSummary(row.AddressSnapshot)
	shopName := strings.TrimSpace(shop.Name)
	if shopName == "" {
		shopName = "历史门店"
	}
	return StoreOrderSummaryDTO{
		ID: idString(row.ID), OrderNo: row.OrderNo, ShopID: idString(row.ShopID),
		OrderType:       normalizedStoreOrderType(row.OrderType),
		SettlementMode:  normalizedStoreSettlementMode(row.SettlementMode),
		SettlementLabel: storeSettlementLabel(row.OrderType, row.SettlementMode),
		Status:          row.Status, PayStatus: row.PayStatus, DeliveryStatus: row.DeliveryStatus,
		PayableAmount: row.PayableAmount,
		ShopSummary:   StoreShopSummaryDTO{ID: idString(row.ShopID), Name: shopName},
		ItemSummary:   itemSummary, ItemKindCount: len(rows), TotalQuantity: totalQuantity,
		AddressSummary: address, CustomerContactMask: contactMask,
		HasRemark: strings.TrimSpace(stringValue(row.Remark)) != "", Version: row.Version,
		CreatedAt: row.CreatedAt.Format(time.RFC3339), UpdatedAt: row.UpdatedAt.Format(time.RFC3339), PaidAt: timeString(row.PaidAt),
		ScheduledStartAt: timeString(row.DeliveryScheduledStartAt),
		ScheduledEndAt:   timeString(row.DeliveryScheduledEndAt),
		NotBeforeAt:      timeString(row.DeliveryNotBeforeAt),
	}
}

// orderItemDTO 返回订单明细DTO。
func orderItemDTO(row OrderItem) OrderItemDTO {
	snapshot := decodeOrderProductSnapshot(row.ProductSnapshot)
	return OrderItemDTO{
		ID:              idString(row.ID),
		ShopProductID:   idString(row.ShopProductID),
		ProductID:       idString(row.ProductID),
		Name:            snapshot.Name,
		BrandName:       snapshot.BrandName,
		Spec:            snapshot.Spec,
		ImageURL:        snapshot.ImageURL,
		Quantity:        row.Quantity,
		SalePriceAmount: row.SalePriceAmount,
		TotalAmount:     row.TotalAmount,
	}
}

type orderProductSnapshot struct {
	Name          string `json:"name"`
	BrandName     string `json:"brand_name"`
	Spec          string `json:"spec"`
	ImageURL      string `json:"image_url"`
	AgeRestricted bool   `json:"age_restricted"`
}

func decodeOrderProductSnapshot(raw datatypes.JSON) orderProductSnapshot {
	var snapshot orderProductSnapshot
	_ = json.Unmarshal(raw, &snapshot)
	return snapshot
}

func storeOrderDetailDTO(row Order, shop Shop, orderItems []OrderItem, payment Payment, hasPayment bool, delivery DeliveryOrder, hasDelivery bool, logs []OrderLog) StoreOrderDetailDTO {
	items := make([]OrderItemDTO, 0, len(orderItems))
	totalQuantity := 0
	var itemSummary StoreOrderItemSummaryDTO
	for index, orderItem := range orderItems {
		item := orderItemDTO(orderItem)
		items = append(items, item)
		totalQuantity += item.Quantity
		if index == 0 {
			itemSummary = StoreOrderItemSummaryDTO{
				ProductID: item.ProductID,
				Name:      item.Name,
				Spec:      item.Spec,
				ImageURL:  item.ImageURL,
				Quantity:  item.Quantity,
			}
		}
	}
	address, contactMask := storeAddressProjection(row.AddressSnapshot)
	result := StoreOrderDetailDTO{
		ID:                  idString(row.ID),
		OrderNo:             row.OrderNo,
		OrderType:           normalizedStoreOrderType(row.OrderType),
		SettlementMode:      normalizedStoreSettlementMode(row.SettlementMode),
		SettlementLabel:     storeSettlementLabel(row.OrderType, row.SettlementMode),
		ShopID:              idString(row.ShopID),
		Status:              row.Status,
		PayStatus:           row.PayStatus,
		DeliveryStatus:      row.DeliveryStatus,
		PayableAmount:       row.PayableAmount,
		ShopSummary:         StoreShopSummaryDTO{ID: idString(shop.ID), Name: shop.Name},
		ItemSummary:         itemSummary,
		ItemKindCount:       len(items),
		TotalQuantity:       totalQuantity,
		CreatedAt:           row.CreatedAt.Format(time.RFC3339),
		UpdatedAt:           row.UpdatedAt.Format(time.RFC3339),
		Items:               items,
		AddressSnapshot:     address,
		Remark:              stringValue(row.Remark),
		DeliveryPromise:     storeDeliveryPromiseProjection(row.DeliveryPromiseSnapshot),
		ComplianceSummary:   storeComplianceProjection(row.ComplianceSnapshot, orderItems),
		GoodsAmount:         row.GoodsAmount,
		DiscountAmount:      row.DiscountAmount,
		DeliveryFeeAmount:   row.DeliveryFeeAmount,
		PaidAmount:          row.PaidAmount,
		CancelSource:        stringValue(row.CancelSource),
		CancelReasonCode:    stringValue(row.CancelReasonCode),
		Version:             row.Version,
		ExpiresAt:           timeString(row.ExpiresAt),
		PaidAt:              timeString(row.PaidAt),
		CancelledAt:         timeString(row.CancelledAt),
		CompletedAt:         timeString(row.CompletedAt),
		CustomerContactMask: contactMask,
		RecentLogs:          storeOrderLogSummaries(logs),
	}
	if hasPayment {
		result.PaymentSummary = &StorePaymentSummaryDTO{
			PaymentNo: payment.PaymentNo, Status: payment.Status, Amount: payment.Amount,
			Currency: payment.Currency, RefundedAmount: payment.RefundedAmount,
			Channel: payment.Channel, Provider: payment.Provider,
			ExpiresAt: timeString(payment.ExpiresAt), PaidAt: timeString(payment.PaidAt),
		}
	}
	if hasDelivery {
		pickupReadyStatus := delivery.PickupReadyStatus
		if pickupReadyStatus == "" {
			pickupReadyStatus = "waiting_store"
			if row.Status == "ready_for_pickup" || row.Status == "delivering" || row.Status == "completed" {
				pickupReadyStatus = "ready"
			}
		}
		result.DeliverySummary = &StoreDeliverySummaryDTO{
			DeliveryOrderID: idString(delivery.ID), RiderID: optionalIDString(delivery.RiderID),
			Status: delivery.Status, PickupReadyStatus: pickupReadyStatus, AssignmentVersion: delivery.AssignmentVersion,
			ScheduledStartAt: timeString(delivery.ScheduledStartAt),
			ScheduledEndAt:   timeString(delivery.ScheduledEndAt),
			NotBeforeAt:      timeString(delivery.NotBeforeAt),
		}
		result.ScheduledStartAt = timeString(delivery.ScheduledStartAt)
		result.ScheduledEndAt = timeString(delivery.ScheduledEndAt)
		result.NotBeforeAt = timeString(delivery.NotBeforeAt)
	}
	return result
}

func normalizedStoreOrderType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "retail"
	}
	return value
}

func normalizedStoreSettlementMode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "cash"
	}
	return value
}

func storeSettlementLabel(orderType, settlementMode string) string {
	if normalizedStoreOrderType(orderType) == "wine_ticket_redemption" &&
		normalizedStoreSettlementMode(settlementMode) == "wine_ticket" {
		return "酒票已核销，本单无需收款"
	}
	return "现金支付"
}

type storedOrderAddressSnapshot struct {
	ContactName      string `json:"contact_name"`
	ContactPhone     string `json:"contact_phone"`
	Province         string `json:"province"`
	City             string `json:"city"`
	CityCode         string `json:"city_code"`
	District         string `json:"district"`
	DistrictCode     string `json:"district_code"`
	AddressDetail    string `json:"address_detail"`
	Doorplate        string `json:"doorplate"`
	FormattedAddress string `json:"formatted_address"`
	AddressVersion   uint32 `json:"address_version"`
}

func storeAddressProjection(raw datatypes.JSON) (StoreOrderAddressSnapshotDTO, string) {
	var stored storedOrderAddressSnapshot
	_ = json.Unmarshal(raw, &stored)
	quality := "legacy_incomplete"
	if strings.TrimSpace(stored.CityCode) != "" && strings.TrimSpace(stored.FormattedAddress) != "" {
		quality = "complete"
	}
	version := stored.AddressVersion
	if version == 0 {
		version = 1
	}
	return StoreOrderAddressSnapshotDTO{
		SnapshotQuality: quality, ContactNameMask: maskContactName(stored.ContactName),
		Province: stored.Province, City: stored.City, CityCode: optionalTrimmedString(stored.CityCode),
		District: stored.District, DistrictCode: optionalTrimmedString(stored.DistrictCode),
		AddressDetail: stored.AddressDetail, Doorplate: optionalTrimmedString(stored.Doorplate),
		FormattedAddress: optionalTrimmedString(stored.FormattedAddress), AddressVersion: version,
	}, maskContactPhone(stored.ContactPhone)
}

type storedDeliveryPromiseEnvelope struct {
	SchemaVersion      uint32          `json:"schema_version"`
	ServiceAreaVersion uint32          `json:"service_area_version"`
	SelectionSource    string          `json:"selection_source"`
	ResolvedAt         string          `json:"resolved_at"`
	DeliveryPromise    json.RawMessage `json:"delivery_promise"`
}

func storeDeliveryPromiseProjection(raw datatypes.JSON) *StoreDeliveryPromiseDTO {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var envelope storedDeliveryPromiseEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}
	payload := []byte(raw)
	if len(envelope.DeliveryPromise) > 0 && string(envelope.DeliveryPromise) != "null" {
		payload = envelope.DeliveryPromise
	}
	var result StoreDeliveryPromiseDTO
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil
	}
	if result.SchemaVersion == 0 {
		result.SchemaVersion = envelope.SchemaVersion
	}
	if result.ServiceAreaVersion == 0 {
		result.ServiceAreaVersion = envelope.ServiceAreaVersion
	}
	if result.SelectionSource == "" {
		result.SelectionSource = envelope.SelectionSource
	}
	if result.ResolvedAt == "" {
		result.ResolvedAt = envelope.ResolvedAt
	}
	switch result.RouteSource {
	case "", "amap", "cache", "local_distance":
	default:
		result.RouteSource = ""
	}
	return &result
}

type storedComplianceSnapshot struct {
	PolicyVersion               string            `json:"policy_version"`
	Status                      string            `json:"status"`
	AdultResult                 string            `json:"adult_result"`
	VerificationLevel           string            `json:"verification_level"`
	CheckedAt                   string            `json:"checked_at"`
	WouldAllow                  bool              `json:"would_allow"`
	AgeRestrictedShopProductIDs []json.RawMessage `json:"age_restricted_shop_product_ids"`
}

func storeComplianceProjection(raw datatypes.JSON, items []OrderItem) StoreComplianceSummaryDTO {
	ageRestricted := false
	for _, item := range items {
		if decodeOrderProductSnapshot(item.ProductSnapshot).AgeRestricted {
			ageRestricted = true
			break
		}
	}
	var stored storedComplianceSnapshot
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &stored)
		ageRestricted = ageRestricted || len(stored.AgeRestrictedShopProductIDs) > 0
	}
	status := "not_required"
	if ageRestricted {
		status = "legacy_unknown"
		if stored.Status == "verified" && stored.AdultResult == "adult" && stored.WouldAllow {
			status = "verified"
		}
	}
	return StoreComplianceSummaryDTO{
		AgeRestricted: ageRestricted, Status: status, PolicyVersion: stored.PolicyVersion,
		VerificationLevel: stored.VerificationLevel, CheckedAt: stored.CheckedAt,
	}
}

func storeOrderLogSummaries(rows []OrderLog) []StoreOrderLogSummaryDTO {
	result := make([]StoreOrderLogSummaryDTO, 0, len(rows))
	for _, row := range rows {
		result = append(result, StoreOrderLogSummaryDTO{
			ID: idString(row.ID), Action: row.Action, ActorType: row.ActorType,
			FromStatus: stringValue(row.FromStatus), ToStatus: stringValue(row.ToStatus),
			CreatedAt: row.CreatedAt.Format(time.RFC3339),
		})
	}
	return result
}

func maskContactName(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return "*"
	}
	if len(runes) == 1 {
		return "*"
	}
	return string(runes[0]) + strings.Repeat("*", len(runes)-1)
}

func maskContactPhone(value string) string {
	runes := []rune(strings.TrimSpace(value))
	switch {
	case len(runes) >= 11:
		return string(runes[:3]) + "****" + string(runes[len(runes)-4:])
	case len(runes) >= 7:
		return string(runes[:2]) + "****" + string(runes[len(runes)-2:])
	case len(runes) >= 3:
		return string(runes[:1]) + "*****" + string(runes[len(runes)-1:])
	default:
		return "*******"
	}
}

func optionalTrimmedString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func optionalIDString(value *uint64) string {
	if value == nil || *value == 0 {
		return ""
	}
	return idString(*value)
}

func timeString(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339)
}

// shopProductDTO 返回门店商品DTO。
func shopProductDTO(row ShopProductRow) ShopProductDTO {
	return ShopProductDTO{
		ID:                  idString(row.ID),
		ShopProductID:       idString(row.ID),
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
		TotalQty:            row.AvailableQty + row.ReservedQty + row.LockedQty,
		LowStockThreshold:   row.LowStockThreshold,
		LowStock:            row.AvailableQty <= row.LowStockThreshold,
		Version:             row.Version,
		UpdatedAt:           row.UpdatedAt.Format(time.RFC3339),
		AgeRestricted:       row.AgeRestricted,
	}
}

func storedAddressSummary(raw datatypes.JSON) (string, string) {
	var stored storedOrderAddressSnapshot
	_ = json.Unmarshal(raw, &stored)
	address := strings.TrimSpace(stored.FormattedAddress)
	if address == "" {
		address = strings.TrimSpace(stored.AddressDetail)
	}
	runes := []rune(address)
	if len(runes) > 8 {
		address = string(runes[:8]) + "***"
	} else if address != "" {
		address += "***"
	}
	if district := strings.TrimSpace(stored.District); district != "" && !strings.HasPrefix(address, district) {
		address = district + " " + address
	}
	return strings.TrimSpace(address), maskContactPhone(stored.ContactPhone)
}

// pickupSnapshot 返回取货快照。
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

// containsID 判断集合是否包含指定 ID。
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
