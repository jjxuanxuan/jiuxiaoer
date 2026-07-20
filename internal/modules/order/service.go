package order

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/compliance"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg         config.Config
	repo        *Repository
	idStore     *idempotency.Store
	idGen       *snowflake.Generator
	payment     PaymentProvider
	metrics     *metrics.Registry
	serviceArea *servicearea.Service
	dispatch    *dispatch.Service
	locations   *customerlocation.Service
	incidents   IncidentResolver
	log         *slog.Logger
}

type IncidentResolver interface {
	ResolveActiveLocked(ctx context.Context, tx *gorm.DB, deliveryID uint64, stage, resolutionCode string) error
}

func (s *Service) WithCustomerLocation(service *customerlocation.Service) *Service {
	s.locations = service
	return s
}

// WithServiceArea 设置服务 Area并返回更新后的值。
func (s *Service) WithServiceArea(service *servicearea.Service) *Service {
	s.serviceArea = service
	return s
}

// WithPaymentProvider 设置支付提供器并返回更新后的值。
func (s *Service) WithPaymentProvider(provider PaymentProvider, registry *metrics.Registry) *Service {
	s.payment = provider
	s.metrics = registry
	return s
}

func (s *Service) paymentCode() string {
	if s.payment == nil {
		return ""
	}
	return s.payment.Code()
}

// WithDispatch 设置调度并返回更新后的值。
func (s *Service) WithDispatch(service *dispatch.Service) *Service {
	s.dispatch = service
	return s
}

func (s *Service) WithIncidentResolver(resolver IncidentResolver) *Service {
	s.incidents = resolver
	return s
}

func (s *Service) WithLogger(log *slog.Logger) *Service {
	if log != nil {
		s.log = log
	}
	return s
}

// NewService 组装订单写入、幂等和 Snowflake ID 生成能力。
func NewService(cfg config.Config, db *gorm.DB, idGen *snowflake.Generator) *Service {
	return &Service{
		cfg:     cfg,
		repo:    NewRepository(db),
		idStore: idempotency.NewStore(db),
		idGen:   idGen,
		log:     slog.Default(),
	}
}

// Create 在同一个 MySQL 事务里创建待支付订单并预占库存。
// 只有幂等响应也写入成功后，这次下单才算整体提交成功。
func (s *Service) Create(ctx context.Context, claims *auth.Claims, method string, path string, key string, req OrderCreateReq) (OrderCreateResp, error) {
	if !s.cfg.Feature.OrderIdempotencyEnabled {
		return OrderCreateResp{}, problem.New(503, "ORDER_IDEMPOTENCY_DISABLED", "Service Unavailable", "order creation is temporarily disabled")
	}
	if !s.cfg.Feature.StockReserveEnabled {
		return OrderCreateResp{}, problem.New(503, "STOCK_RESERVE_DISABLED", "Service Unavailable", "stock reservation is temporarily disabled")
	}
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return OrderCreateResp{}, err
	}
	shopID, err := parseID(req.ShopID)
	if err != nil {
		return OrderCreateResp{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop_id")
	}
	addressID, err := parseID(req.AddressID)
	if err != nil {
		return OrderCreateResp{}, problem.InvalidArgument("ORDER_INVALID_ADDRESS", "invalid address_id")
	}
	if len(req.Items) == 0 {
		return OrderCreateResp{}, problem.InvalidArgument("ORDER_EMPTY_ITEMS", "order items required")
	}
	requestHash := idempotency.RequestHash(req)
	var replay OrderCreateResp
	if cached, replayErr := s.idStore.ReplayCompleted(ctx, s.repo.DB(), claims.AccountType, customerID, path, key, requestHash, &replay); replayErr != nil {
		return OrderCreateResp{}, replayErr
	} else if cached {
		return replay, nil
	}

	var locationResolution *customerlocation.OrderResolution
	if s.cfg.CustomerLBS.Mode != "off" {
		if s.locations == nil {
			if s.cfg.CustomerLBS.Mode == "enforce" {
				return OrderCreateResp{}, problem.Internal("customer location resolver unavailable")
			}
		} else {
			value, resolveErr := s.locations.ResolveOrder(ctx, customerID, addressID)
			if resolveErr != nil {
				if s.cfg.CustomerLBS.Mode == "enforce" {
					return OrderCreateResp{}, resolveErr
				}
			} else {
				locationResolution = &value
				if !contextContainsShop(value.Context, req.ShopID) && s.cfg.CustomerLBS.Mode == "enforce" {
					return OrderCreateResp{}, serviceShopChanged(value.Context)
				}
			}
		}
	}

	var resp OrderCreateResp
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先占用幂等键再修改库存，避免重复提交导致库存被预占两次。
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &resp)
			if err != nil {
				return err
			}
			if cached {
				return nil
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}

		addressRow, err := s.repo.GetAddress(ctx, tx, customerID, addressID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.InvalidArgument("ORDER_INVALID_ADDRESS", "address not found")
		}
		if err != nil {
			return err
		}

		var resolved *servicearea.ResolveDTO
		selectionSource := "automatic"
		if locationResolution != nil {
			if addressRow.Version != locationResolution.AddressVersion {
				return problem.Conflict("ADDRESS_VERSION_CONFLICT", "address changed while the order was being prepared")
			}
			currentRows, currentErr := s.serviceArea.CandidatesWithDB(ctx, tx, servicearea.ResolveInput{CityCode: stringValue(addressRow.CityCode), Latitude: pointerValue(addressRow.Latitude), Longitude: pointerValue(addressRow.Longitude)}, 5)
			if currentErr != nil {
				if s.cfg.CustomerLBS.Mode == "enforce" {
					return currentErr
				}
			} else {
				var chosen *servicearea.ShopDTO
				for _, candidate := range locationResolution.Context.CandidateShops {
					if candidate.ID != req.ShopID {
						continue
					}
					for _, current := range currentRows {
						if strconv.FormatUint(current.ID, 10) == req.ShopID && current.ServiceAreaVersion == candidate.ServiceAreaVersion {
							copy := candidate
							chosen = &copy
						}
					}
				}
				if chosen == nil && s.cfg.CustomerLBS.Mode == "enforce" {
					return serviceShopChanged(locationResolution.Context)
				}
				if chosen != nil {
					if locationResolution.Context.ServiceShop == nil || locationResolution.Context.ServiceShop.ID != req.ShopID {
						selectionSource = "manual"
					}
					resolved = &servicearea.ResolveDTO{ServiceShop: *chosen, ResolvedAt: time.Now().UTC()}
				}
			}
		} else if s.cfg.Service.EnforcementMode != "off" {
			if s.serviceArea == nil {
				if s.cfg.Service.EnforcementMode == "enforce" {
					return problem.Internal("service area resolver unavailable")
				}
			} else if addressRow.CityCode == nil || addressRow.Latitude == nil || addressRow.Longitude == nil {
				if s.cfg.Service.EnforcementMode == "enforce" {
					return problem.New(422, "LOCATION_REQUIRED", "Unprocessable Entity", "address requires city_code and coordinates")
				}
			} else {
				value, resolveErr := s.serviceArea.ResolveWithDB(ctx, tx, servicearea.ResolveInput{CityCode: *addressRow.CityCode, Latitude: *addressRow.Latitude, Longitude: *addressRow.Longitude})
				if resolveErr != nil {
					if s.cfg.Service.EnforcementMode == "enforce" {
						return resolveErr
					}
				} else {
					resolved = &value
					if value.ServiceShop.ID != req.ShopID && s.cfg.Service.EnforcementMode == "enforce" {
						detail := problem.Conflict("SERVICE_SHOP_CHANGED", "requested shop does not match the current service shop")
						detail.Data = map[string]any{"service_shop": value.ServiceShop}
						return detail
					}
				}
			}
		}

		aggregated, err := aggregateItems(req.Items)
		if err != nil {
			return err
		}

		orderID := s.idGen.Next()
		orderNo := orderNo(orderID)
		expiresAt := time.Now().Add(s.cfg.Order.PaymentTTL)
		var merchantID uint64
		var goodsAmount int64
		orderItems := make([]OrderItem, 0, len(aggregated))

		shopProductIDs := make([]uint64, 0, len(aggregated))
		for shopProductID := range aggregated {
			shopProductIDs = append(shopProductIDs, shopProductID)
		}
		sort.Slice(shopProductIDs, func(i int, j int) bool { return shopProductIDs[i] < shopProductIDs[j] })
		// Run the alcohol compliance gate before the first inventory write.
		complianceSnapshot, err := compliance.CheckOrder(ctx, tx, s.cfg.CP1, customerID, shopProductIDs)
		if err != nil {
			return err
		}
		for _, shopProductID := range shopProductIDs {
			quantity := aggregated[shopProductID]
			// 商品、门店和库存逐项校验，因为 shop_products 承载商户和门店范围。
			productRow, err := s.repo.GetShopProductForOrder(ctx, tx, shopID, shopProductID)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.Conflict("PRODUCT_NOT_ON_SALE", "product not on sale")
			}
			if err != nil {
				return err
			}
			if productRow.ShopProductStatus != "on_sale" || productRow.ProductStatus != "on_sale" || productRow.ShopStatus != "active" || productRow.BusinessStatus != "open" {
				return problem.Conflict("PRODUCT_NOT_ON_SALE", "product not on sale")
			}
			if merchantID == 0 {
				merchantID = productRow.MerchantID
			}

			// 行锁保证并发下单不会把同一个门店商品卖超。
			stock, err := s.repo.LockStock(ctx, tx, shopProductID)
			if errors.Is(err, gorm.ErrRecordNotFound) || stock.AvailableQty < quantity {
				return problem.Conflict("STOCK_NOT_ENOUGH", "stock not enough")
			}
			if err != nil {
				return err
			}
			beforeAvailable := stock.AvailableQty
			if err := s.repo.ReserveStock(ctx, tx, stock, quantity); err != nil {
				return err
			}
			stockRecordID := s.idGen.Next()
			if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{
				ID:                 stockRecordID,
				ShopProductID:      shopProductID,
				ShopID:             shopID,
				ProductID:          productRow.ProductID,
				ChangeType:         "reserve",
				QuantityDelta:      -quantity,
				BeforeAvailableQty: beforeAvailable,
				AfterAvailableQty:  beforeAvailable - quantity,
				SourceType:         "order",
				SourceID:           orderID,
				IdempotencyKey:     stringPtr(key),
			}); err != nil {
				return err
			}
			if err := s.createOutbox(ctx, tx, "stock.reserved", "stock_record", stockRecordID, map[string]any{"stock_record_id": idString(stockRecordID), "order_id": idString(orderID)}); err != nil {
				return err
			}

			// 快照商品展示字段，避免后续目录修改影响历史订单。
			totalAmount := int64(quantity) * productRow.SalePriceAmount
			goodsAmount += totalAmount
			orderItems = append(orderItems, OrderItem{
				ID:              s.idGen.Next(),
				OrderID:         orderID,
				ShopProductID:   shopProductID,
				ProductID:       productRow.ProductID,
				ProductSnapshot: jsonData(productSnapshot(productRow)),
				Quantity:        quantity,
				SalePriceAmount: productRow.SalePriceAmount,
				TotalAmount:     totalAmount,
			})
		}

		deliveryFee := int64(0)
		var deliveryPromiseSnapshot datatypes.JSON
		if resolved != nil && resolved.ServiceShop.ID == req.ShopID {
			promise := resolved.ServiceShop.DeliveryPromise
			deliveryFee = promise.DeliveryFeeAmount
			if promise.FreeDeliveryThresholdAmount != nil && goodsAmount >= *promise.FreeDeliveryThresholdAmount {
				deliveryFee = 0
			}
			deliveryPromiseSnapshot = jsonData(map[string]any{
				"schema_version": 2, "service_area_version": resolved.ServiceShop.ServiceAreaVersion,
				"selection_source": selectionSource, "distance_m": resolved.ServiceShop.DistanceM, "resolved_at": resolved.ResolvedAt,
				"route": map[string]any{
					"provider": routeProvider(promise.RouteSource), "resolution_source": promise.RouteSource,
					"degraded": resolved.ServiceShop.Degraded, "distance_m": promise.RouteDistanceM,
					"duration_seconds": promise.RouteDurationSeconds, "planned_at": resolved.ResolvedAt,
				},
				"delivery_promise": promise,
			})
		}
		orderRow := Order{
			ID:                      orderID,
			OrderNo:                 orderNo,
			CustomerID:              customerID,
			MerchantID:              merchantID,
			ShopID:                  shopID,
			Status:                  "pending_payment",
			PayStatus:               "pending",
			DeliveryStatus:          "pending",
			GoodsAmount:             goodsAmount,
			DiscountAmount:          0,
			DeliveryFeeAmount:       deliveryFee,
			PayableAmount:           goodsAmount + deliveryFee,
			PaidAmount:              0,
			Remark:                  optionalString(req.Remark),
			AddressSnapshot:         jsonData(addressSnapshot(addressRow)),
			DeliveryPromiseSnapshot: deliveryPromiseSnapshot,
			ComplianceSnapshot:      complianceSnapshot,
			IdempotencyKey:          stringPtr(key),
			ExpiresAt:               &expiresAt,
		}
		if err := s.repo.CreateOrder(ctx, tx, orderRow); err != nil {
			return err
		}
		if err := s.repo.CreateOrderItems(ctx, tx, orderItems); err != nil {
			return err
		}
		if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{
			ID:        s.idGen.Next(),
			OrderID:   orderID,
			ActorType: claims.AccountType,
			ActorID:   customerID,
			Action:    "create",
			ToStatus:  stringPtr("pending_payment"),
			RequestID: requestctx.RequestIDPtr(ctx),
		}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, claims.AccountType, customerID, "order.create", "order", orderID, nil, orderRow); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "order.created", "order", orderID, map[string]any{"order_id": idString(orderID), "order_no": orderNo}); err != nil {
			return err
		}
		// 订单、库存预占和购物车清理必须一起提交。只删除本次订单里的
		// shop_product，未提交（包括未选中）的购物车商品继续保留。
		if err := s.repo.DeletePurchasedCartItems(ctx, tx, customerID, shopProductIDs); err != nil {
			return err
		}

		resp = OrderCreateResp{
			OrderID:       idString(orderID),
			OrderNo:       orderNo,
			Status:        "pending_payment",
			PayableAmount: goodsAmount + deliveryFee,
			ExpiresAt:     expiresAt.Format(time.RFC3339),
			DeliveryPromise: func() any {
				if resolved == nil {
					return nil
				}
				return resolved.ServiceShop.DeliveryPromise
			}(),
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, resp)
	})
	return resp, err
}

// List 查询订单 DTO列表列表。
func (s *Service) List(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]OrderDTO, string, error) {
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.repo.ListCustomerOrders(ctx, customerID, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		nextPageToken = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]OrderDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, orderDTO(row, nil))
	}
	return items, nextPageToken, nil
}

// Detail 返回Detail。
func (s *Service) Detail(ctx context.Context, claims *auth.Claims, orderIDRaw string) (OrderDTO, error) {
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return OrderDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return OrderDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}
	row, items, err := s.repo.GetCustomerOrder(ctx, customerID, orderID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OrderDTO{}, problem.NotFound("ORDER_NOT_FOUND", "order not found")
	}
	if err != nil {
		return OrderDTO{}, err
	}
	return orderDTO(row, items), nil
}

// Cancel 在 P0 阶段刻意限制为只能取消待支付订单。
// 已支付订单取消需要退款流程，暂不进入第一版。
func (s *Service) Cancel(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string, req OrderCancelReq) (OrderDTO, error) {
	actor, err := cancelActorFromClaims(claims)
	if err != nil {
		return OrderDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return OrderDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}

	var resp OrderDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, actor.ActorID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, actor.ActorID, path, key, &resp)
			if err != nil {
				return err
			}
			if cached {
				return nil
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}

		var row Order
		if actor.IsAdmin {
			row, err = s.repo.LockOrder(ctx, tx, orderID)
		} else {
			row, err = s.repo.LockCustomerOrder(ctx, tx, actor.CustomerID, orderID)
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ORDER_NOT_FOUND", "order not found")
		}
		if err != nil {
			return err
		}
		if req.ExpectedVersion != 0 && uint(row.Version) != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "order version changed")
		}
		if row.Status != "pending_payment" {
			if !actor.IsAdmin || row.PayStatus != "paid" || row.Status == "completed" || row.Status == "cancelled" || row.Status == "refunded" || row.Status == "refunding" {
				return problem.Conflict("ORDER_INVALID_STATUS", "order cannot be cancelled")
			}
			var cancelErr error
			resp, cancelErr = s.cancelPaidOrder(ctx, tx, row, actor.ActorID, req)
			if cancelErr != nil {
				return cancelErr
			}
			return s.idStore.Succeed(ctx, tx, claims.AccountType, actor.ActorID, path, key, resp)
		}
		source := "customer"
		reasonCode := "CUSTOMER_CANCELLED"
		if actor.IsAdmin {
			source = "admin"
			reasonCode = "ADMIN_CANCELLED"
		}
		resp, err = s.cancelPendingOrder(ctx, tx, row, claims.AccountType, actor.ActorID, source, reasonCode, req.Reason, key)
		if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, actor.ActorID, path, key, resp)
	})
	return resp, err
}

// cancelPaidOrder 取消Paid 订单。
// cancelPaidOrder never marks money refunded. It moves the order into the
// existing refund workflow boundary and emits a durable request for the refund
// orchestrator, while cancelling any open delivery assignment.
func (s *Service) cancelPaidOrder(ctx context.Context, tx *gorm.DB, row Order, actorID uint64, req OrderCancelReq) (OrderDTO, error) {
	now := time.Now()
	reasonCode := req.ReasonCode
	if reasonCode == "" {
		reasonCode = "ADMIN_CANCELLED"
	}
	var pickedUp int64
	if err := tx.WithContext(ctx).Table("delivery_orders").Where("order_id=? AND (picked_up_at IS NOT NULL OR status IN ('delivering','completed')) AND deleted_at IS NULL", row.ID).Count(&pickedUp).Error; err != nil {
		return OrderDTO{}, err
	}
	if pickedUp > 0 {
		return OrderDTO{}, problem.Conflict("DELIVERY_ALREADY_PICKED_UP", "picked-up delivery must be handled through the fulfillment exception flow")
	}
	if err := s.repo.UpdateOrder(ctx, tx, row.ID, map[string]any{"status": "refunding", "delivery_status": "cancelled", "cancel_source": "admin", "cancel_reason_code": reasonCode, "cancelled_at": &now, "version": gorm.Expr("version + 1")}); err != nil {
		return OrderDTO{}, err
	}
	var deliveryIDs []uint64
	if err := tx.WithContext(ctx).Table("delivery_orders").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("order_id=?", row.ID).Order("id").Scan(&deliveryIDs).Error; err != nil {
		return OrderDTO{}, err
	}
	if err := tx.WithContext(ctx).Table("delivery_orders").Where("order_id = ? AND status NOT IN ?", row.ID, []string{"completed", "cancelled"}).Updates(map[string]any{"status": "cancelled", "dispatch_status": "cancelled", "pickup_ready_status": "cancelled", "cancelled_at": &now}).Error; err != nil {
		return OrderDTO{}, err
	}
	if len(deliveryIDs) > 0 {
		if s.incidents != nil {
			for _, deliveryID := range deliveryIDs {
				if err := s.incidents.ResolveActiveLocked(ctx, tx, deliveryID, "", "order_cancelled"); err != nil {
					return OrderDTO{}, err
				}
			}
		}
		if err := tx.WithContext(ctx).Table("dispatch_jobs").Where("delivery_order_id IN ? AND status IN ?", deliveryIDs, []string{"pending", "scoring", "offering", "grab_open", "manual_required"}).Updates(map[string]any{"status": "cancelled", "status_reason_code": "ORDER_CANCELLED", "locked_by": nil, "locked_until": nil, "version": gorm.Expr("version+1")}).Error; err != nil {
			return OrderDTO{}, err
		}
		if err := tx.WithContext(ctx).Table("dispatch_offers").Where("delivery_order_id IN ? AND status='pending'", deliveryIDs).Updates(map[string]any{"status": "cancelled", "responded_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
			return OrderDTO{}, err
		}
		if err := tx.WithContext(ctx).Table("delivery_assignments").Where("delivery_order_id IN ? AND status='active'", deliveryIDs).Update("status", "cancelled").Error; err != nil {
			return OrderDTO{}, err
		}
		if err := tx.WithContext(ctx).Table("delivery_verifications").Where("delivery_order_id IN ? AND status IN ?", deliveryIDs, []string{"active", "locked"}).Update("status", "cancelled").Error; err != nil {
			return OrderDTO{}, err
		}
	}
	if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{ID: s.idGen.Next(), OrderID: row.ID, ActorType: "admin", ActorID: actorID, Action: "admin_cancel_paid", FromStatus: stringPtr(row.Status), ToStatus: stringPtr("refunding"), Remark: optionalString(req.Reason), RequestID: requestctx.RequestIDPtr(ctx)}); err != nil {
		return OrderDTO{}, err
	}
	if err := s.createAudit(ctx, tx, "admin", actorID, "order.cancel_paid", "order", row.ID, row, map[string]any{"status": "refunding", "reason_code": reasonCode}); err != nil {
		return OrderDTO{}, err
	}
	if err := s.createOutbox(ctx, tx, "admin.order.refund_requested", "order", row.ID, map[string]any{"order_id": idString(row.ID), "amount": row.PaidAmount, "reason_code": reasonCode}); err != nil {
		return OrderDTO{}, err
	}
	if err := s.createOutbox(ctx, tx, "order.cancelled", "order", row.ID, map[string]any{"order_id": idString(row.ID), "source": "admin", "refund_pending": true}); err != nil {
		return OrderDTO{}, err
	}
	row.Status = "refunding"
	row.DeliveryStatus = "cancelled"
	row.CancelSource = stringPtr("admin")
	row.CancelReasonCode = stringPtr(reasonCode)
	row.CancelledAt = &now
	row.Version++
	items, err := s.repo.OrderItems(ctx, tx, row.ID)
	if err != nil {
		return OrderDTO{}, err
	}
	return orderDTO(row, items), nil
}

// cancelPendingOrder 取消待处理订单。
func (s *Service) cancelPendingOrder(ctx context.Context, tx *gorm.DB, row Order, actorType string, actorID uint64, source string, reasonCode string, reasonText string, idempotencyKey string) (OrderDTO, error) {
	now := time.Now()
	if err := s.repo.ClosePendingPayments(ctx, tx, row.ID, now); err != nil {
		return OrderDTO{}, err
	}
	items, err := s.repo.OrderItems(ctx, tx, row.ID)
	if err != nil {
		return OrderDTO{}, err
	}
	for _, item := range items {
		stock, err := s.repo.LockStock(ctx, tx, item.ShopProductID)
		if err != nil {
			return OrderDTO{}, err
		}
		if stock.ReservedQty < item.Quantity {
			return OrderDTO{}, problem.Conflict("STOCK_RESERVATION_INCONSISTENT", "reserved stock is lower than the order quantity")
		}
		beforeAvailable := stock.AvailableQty
		if err := s.repo.ReleaseStock(ctx, tx, stock, item.Quantity); err != nil {
			return OrderDTO{}, err
		}
		stockRecordID := s.idGen.Next()
		if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{
			ID:                 stockRecordID,
			ShopProductID:      item.ShopProductID,
			ShopID:             row.ShopID,
			ProductID:          item.ProductID,
			ChangeType:         "release",
			QuantityDelta:      item.Quantity,
			BeforeAvailableQty: beforeAvailable,
			AfterAvailableQty:  beforeAvailable + item.Quantity,
			SourceType:         "order",
			SourceID:           row.ID,
			IdempotencyKey:     stringPtr(idempotencyKey),
		}); err != nil {
			return OrderDTO{}, err
		}
		if err := s.createOutbox(ctx, tx, "stock.released", "stock_record", stockRecordID, map[string]any{"stock_record_id": idString(stockRecordID), "order_id": idString(row.ID), "reason_code": reasonCode}); err != nil {
			return OrderDTO{}, err
		}
	}
	if err := s.repo.UpdateOrder(ctx, tx, row.ID, map[string]any{
		"status":             "cancelled",
		"pay_status":         "closed",
		"cancel_source":      source,
		"cancel_reason_code": reasonCode,
		"cancelled_at":       &now,
		"version":            gorm.Expr("version + 1"),
	}); err != nil {
		return OrderDTO{}, err
	}
	action := "cancel"
	auditAction := "order.cancel"
	if source == "system" {
		action = "expire"
		auditAction = "order.expire"
	}
	if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{
		ID:         s.idGen.Next(),
		OrderID:    row.ID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     action,
		FromStatus: stringPtr(row.Status),
		ToStatus:   stringPtr("cancelled"),
		Remark:     optionalString(reasonText),
		RequestID:  requestctx.RequestIDPtr(ctx),
	}); err != nil {
		return OrderDTO{}, err
	}
	if err := s.createAudit(ctx, tx, actorType, actorID, auditAction, "order", row.ID, row, map[string]any{"status": "cancelled", "reason_code": reasonCode}); err != nil {
		return OrderDTO{}, err
	}
	if err := s.createOutbox(ctx, tx, "order.cancelled", "order", row.ID, map[string]any{"order_id": idString(row.ID), "source": source, "reason_code": reasonCode}); err != nil {
		return OrderDTO{}, err
	}
	row.Status = "cancelled"
	row.PayStatus = "closed"
	row.CancelSource = stringPtr(source)
	row.CancelReasonCode = stringPtr(reasonCode)
	row.CancelledAt = &now
	row.Version++
	return orderDTO(row, items), nil
}

// MockPay 模拟支付成功，并扣减之前预占的库存。
// 它保留 payment 表和 outbox 结构，便于后续替换为真实支付回调。
func (s *Service) MockPay(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string, req MockPayReq) (PaymentDTO, error) {
	if !s.cfg.Feature.PaymentMockEnabled {
		return PaymentDTO{}, problem.Forbidden("PAYMENT_MOCK_DISABLED", "mock payment disabled")
	}
	if req.Channel != "mock" {
		return PaymentDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "channel must be mock")
	}
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return PaymentDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return PaymentDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}

	var resp PaymentDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &resp)
			if err != nil {
				return err
			}
			if cached {
				return nil
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}

		row, err := s.repo.LockCustomerOrder(ctx, tx, customerID, orderID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ORDER_NOT_FOUND", "order not found")
		}
		if err != nil {
			return err
		}
		if row.Status == "paid" || row.PayStatus == "succeeded" {
			return problem.Conflict("PAYMENT_ALREADY_SUCCEEDED", "payment already succeeded")
		}
		if row.Status != "pending_payment" {
			return problem.Conflict("ORDER_INVALID_STATUS", "order cannot be paid")
		}

		items, err := s.repo.OrderItems(ctx, tx, orderID)
		if err != nil {
			return err
		}
		for _, item := range items {
			// 支付消耗 reserved_qty；available_qty 在创建订单时已经减少。
			stock, err := s.repo.LockStock(ctx, tx, item.ShopProductID)
			if err != nil {
				return err
			}
			if stock.ReservedQty < item.Quantity {
				return problem.Conflict("STOCK_NOT_ENOUGH", "reserved stock not enough")
			}
			beforeAvailable := stock.AvailableQty
			if err := s.repo.DeductReservedStock(ctx, tx, stock, item.Quantity); err != nil {
				return err
			}
			stockRecordID := s.idGen.Next()
			if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{
				ID:                 stockRecordID,
				ShopProductID:      item.ShopProductID,
				ShopID:             row.ShopID,
				ProductID:          item.ProductID,
				ChangeType:         "deduct",
				QuantityDelta:      -item.Quantity,
				BeforeAvailableQty: beforeAvailable,
				AfterAvailableQty:  beforeAvailable,
				SourceType:         "payment",
				SourceID:           orderID,
				IdempotencyKey:     stringPtr(key),
			}); err != nil {
				return err
			}
			if err := s.createOutbox(ctx, tx, "stock.deducted", "stock_record", stockRecordID, map[string]any{"stock_record_id": idString(stockRecordID), "order_id": idString(orderID)}); err != nil {
				return err
			}
		}

		now := time.Now()
		payment := Payment{
			ID:             s.idGen.Next(),
			PaymentNo:      paymentNo(orderID),
			OrderID:        orderID,
			CustomerID:     customerID,
			Channel:        "mock",
			Provider:       "mock",
			ProviderStatus: stringPtr("SUCCESS"),
			Status:         "succeeded",
			Amount:         row.PayableAmount,
			Currency:       "CNY",
			ExpiresAt:      row.ExpiresAt,
			PaidAt:         &now,
			IdempotencyKey: stringPtr(key),
		}
		if err := s.repo.CreatePayment(ctx, tx, payment); err != nil {
			return err
		}
		if err := s.repo.UpdateOrder(ctx, tx, orderID, map[string]any{
			"status":      "paid",
			"pay_status":  "succeeded",
			"paid_amount": row.PayableAmount,
			"paid_at":     &now,
		}); err != nil {
			return err
		}
		if s.dispatch != nil {
			if _, _, err := s.dispatch.EnsurePaidOrderTask(ctx, tx, dispatch.PaidOrderInput{OrderID: orderID, ShopID: row.ShopID, AddressSnapshot: row.AddressSnapshot}); err != nil {
				return err
			}
		}
		if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{
			ID:         s.idGen.Next(),
			OrderID:    orderID,
			ActorType:  claims.AccountType,
			ActorID:    customerID,
			Action:     "pay",
			FromStatus: stringPtr(row.Status),
			ToStatus:   stringPtr("paid"),
			RequestID:  requestctx.RequestIDPtr(ctx),
		}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, claims.AccountType, customerID, "order.pay", "order", orderID, row, map[string]any{"status": "paid", "payment_id": idString(payment.ID)}); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "order.paid", "order", orderID, map[string]any{"order_id": idString(orderID), "payment_id": idString(payment.ID)}); err != nil {
			return err
		}

		resp = paymentDTO(payment)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, resp)
	})
	return resp, err
}

// CreatePayment 创建支付。
func (s *Service) CreatePayment(ctx context.Context, claims *auth.Claims, method string, path string, key string, orderIDRaw string, req PaymentCreateReq) (PaymentDTO, error) {
	if s.payment == nil || !s.cfg.WeChat.PayEnabled || req.Provider != s.payment.Code() {
		return PaymentDTO{}, problem.New(503, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", "payment provider is unavailable")
	}
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return PaymentDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return PaymentDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}

	var orderRow Order
	var payment Payment
	var openID string
	var response PaymentDTO
	callProvider := false
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &response)
			if err != nil {
				return err
			}
			if cached {
				return nil
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}

		orderRow, err = s.repo.LockCustomerOrder(ctx, tx, customerID, orderID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ORDER_NOT_FOUND", "order not found")
		}
		if err != nil {
			return err
		}
		if orderRow.Status != "pending_payment" || orderRow.PayStatus != "pending" {
			return problem.Conflict("ORDER_INVALID_STATUS", "order cannot create a payment")
		}
		if orderRow.ExpiresAt == nil || !orderRow.ExpiresAt.After(time.Now()) {
			return problem.Conflict("ORDER_PAYMENT_EXPIRED", "order payment window has expired")
		}
		openID, err = s.repo.CustomerWeChatOpenID(ctx, tx, customerID, s.cfg.WeChat.MiniAppID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.Conflict("WECHAT_IDENTITY_REQUIRED", "wechat identity is required for payment")
		}
		if err != nil {
			return err
		}

		payment, err = s.repo.LockPaymentByOrderProvider(ctx, tx, orderID, req.Provider)
		if err == nil {
			if (payment.Status == "pending" || payment.Status == "succeeded") && len(payment.ClientPayload) > 0 {
				response = paymentDTO(payment)
				return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, response)
			}
			if payment.Status == "closed" || payment.Status == "succeeded" {
				return problem.Conflict("ORDER_INVALID_STATUS", "payment cannot be recreated")
			}
			if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{
				"status":          "creating",
				"failure_code":    nil,
				"failed_at":       nil,
				"idempotency_key": key,
				"version":         gorm.Expr("version + 1"),
			}); err != nil {
				return err
			}
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			payment = Payment{
				ID:             s.idGen.Next(),
				PaymentNo:      paymentNo(orderID),
				OrderID:        orderID,
				CustomerID:     customerID,
				Channel:        req.ClientType,
				Provider:       req.Provider,
				Status:         "creating",
				Amount:         orderRow.PayableAmount,
				Currency:       "CNY",
				ExpiresAt:      orderRow.ExpiresAt,
				IdempotencyKey: stringPtr(key),
			}
			if err := s.repo.CreatePayment(ctx, tx, payment); err != nil {
				return err
			}
		} else {
			return err
		}
		callProvider = true
		return nil
	})
	if err != nil || !callProvider {
		return response, err
	}

	providerCtx, cancel := context.WithTimeout(ctx, s.cfg.WeChat.HTTPTimeout)
	providerResult, providerErr := s.payment.Create(providerCtx, CreateProviderPaymentInput{
		PaymentNo:   payment.PaymentNo,
		Description: s.cfg.WeChat.PayDescription,
		Amount:      payment.Amount,
		Currency:    payment.Currency,
		OpenID:      openID,
		ExpiresAt:   *payment.ExpiresAt,
	})
	cancel()
	if providerErr != nil {
		s.logPaymentProviderFailure(payment.PaymentNo, providerErr, "reconcile_or_retry")
		s.metrics.IncPayment(req.Provider, "create_failed")
		next := time.Now()
		_ = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			values := map[string]any{"status": "creating", "next_reconcile_at": &next, "failure_code": paygateway.Code(providerErr, "PROVIDER_UNAVAILABLE"), "version": gorm.Expr("version + 1")}
			if !paygateway.Retryable(providerErr) {
				values["status"] = "failed"
				values["failed_at"] = &next
				values["next_reconcile_at"] = nil
			}
			if err := s.repo.UpdatePayment(ctx, tx, payment.ID, values); err != nil {
				return err
			}
			return s.idStore.Fail(ctx, tx, claims.AccountType, customerID, path, key)
		})
		message := "payment creation was rejected by the provider"
		if paygateway.Retryable(providerErr) {
			message = "payment creation result is unknown; backend reconciliation has been scheduled"
		}
		detail := problem.New(503, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", message)
		detail.Data = map[string]any{"retryable": paygateway.Retryable(providerErr), "provider_code": paygateway.Code(providerErr, "PROVIDER_UNAVAILABLE"), "provider_request_id": paygateway.RequestID(providerErr)}
		return PaymentDTO{}, detail
	}
	s.log.Info("payment provider call completed", slog.String("operation", "payment.create"), slog.String("payment_no", payment.PaymentNo), slog.String("provider_status", providerResult.Status), slog.String("provider_request_id", providerResult.RequestID))

	payload := jsonData(providerResult.ClientPayload)
	nextReconcileAt := nextPaymentReconcileAt(payment, time.Now())
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := s.repo.LockPaymentByOrderProvider(ctx, tx, orderID, req.Provider)
		if err != nil {
			return err
		}
		if locked.Status == "closed" {
			return problem.Conflict("ORDER_PAYMENT_EXPIRED", "order payment window has expired")
		}
		if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{
			"status":             "pending",
			"provider_status":    providerResult.Status,
			"provider_prepay_id": optionalString(providerResult.ProviderPrepayID),
			"provider_trade_no":  optionalString(providerResult.ProviderTradeNo),
			"client_payload":     payload,
			"next_reconcile_at":  nextReconcileAt,
			"failure_code":       nil,
			"version":            gorm.Expr("version + 1"),
		}); err != nil {
			return err
		}
		payment.Status = "pending"
		payment.ProviderStatus = optionalString(providerResult.Status)
		payment.ProviderPrepayID = optionalString(providerResult.ProviderPrepayID)
		payment.ProviderTradeNo = optionalString(providerResult.ProviderTradeNo)
		payment.ClientPayload = payload
		response = paymentDTO(payment)
		if err := s.createOutbox(ctx, tx, "payment.created", "payment", payment.ID, map[string]any{"payment_id": idString(payment.ID), "order_id": idString(orderID), "provider": req.Provider, "provider_request_id": providerResult.RequestID}); err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, response)
	})
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), s.cfg.WeChat.HTTPTimeout)
		closeResult, closeErr := s.payment.Close(closeCtx, payment.PaymentNo)
		cancel()
		if closeErr != nil {
			s.logPaymentProviderFailure(payment.PaymentNo, closeErr, "manual_investigation")
			s.metrics.IncPayment(req.Provider, "create_compensation_failed")
		} else {
			s.log.Info("payment provider call completed", slog.String("operation", "payment.close"), slog.String("payment_no", payment.PaymentNo), slog.String("provider_request_id", closeResult.RequestID))
			s.metrics.IncPayment(req.Provider, "create_compensated")
		}
		return PaymentDTO{}, err
	}
	s.metrics.IncPayment(req.Provider, "create_succeeded")
	return response, nil
}

// GetPayment 获取支付。
func (s *Service) GetPayment(ctx context.Context, claims *auth.Claims, orderIDRaw string) (PaymentDTO, error) {
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return PaymentDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return PaymentDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}
	payment, err := s.repo.GetCustomerPayment(ctx, customerID, orderID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PaymentDTO{}, problem.NotFound("PAYMENT_NOT_FOUND", "payment not found")
	}
	if err != nil {
		return PaymentDTO{}, err
	}
	return paymentDTO(payment), nil
}

// ProcessPaymentCallback 返回Process 支付回调。
func (s *Service) ProcessPaymentCallback(ctx context.Context, providerCode string, request *http.Request, rawBody []byte) error {
	if s.payment == nil || providerCode != s.payment.Code() {
		return problem.NotFound("PAYMENT_PROVIDER_NOT_FOUND", "payment provider not found")
	}
	request.Body = io.NopCloser(bytes.NewReader(rawBody))
	event, err := s.payment.ParseCallback(ctx, request)
	payloadHash := fmt.Sprintf("%x", sha256.Sum256(rawBody))
	if err != nil {
		s.metrics.IncPayment(providerCode, "callback_signature_failed")
		_ = s.recordRejectedCallback(ctx, providerCode, "invalid:"+payloadHash, payloadHash, "SIGNATURE_INVALID", false)
		return problem.Unauthorized("PAYMENT_CALLBACK_INVALID", "payment callback verification failed")
	}
	if !s.cfg.WeChat.PayMockEnabled && (event.AppID != s.cfg.WeChat.MiniAppID || event.MchID != s.cfg.WeChat.PayMchID) {
		s.metrics.IncPayment(providerCode, "callback_identity_mismatch")
		_ = s.recordRejectedCallback(ctx, providerCode, event.EventID, payloadHash, "MERCHANT_IDENTITY_MISMATCH", true)
		return problem.Unauthorized("PAYMENT_CALLBACK_INVALID", "payment callback verification failed")
	}

	var callbackReject error
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		callback := PaymentCallback{
			ID:              s.idGen.Next(),
			Provider:        providerCode,
			ProviderEventID: event.EventID,
			ProviderTradeNo: optionalString(event.ProviderTradeNo),
			PayloadHash:     payloadHash,
			SignatureValid:  true,
			ProcessStatus:   "received",
			ReceivedAt:      time.Now(),
			RequestID:       requestctx.RequestIDPtr(ctx),
		}
		created, err := s.repo.CreatePaymentCallbackIfAbsent(ctx, tx, callback)
		if err != nil || !created {
			return err
		}
		paymentLookup, err := s.repo.GetPaymentByNo(ctx, tx, event.PaymentNo, providerCode)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			now := time.Now()
			return s.repo.UpdatePaymentCallback(ctx, tx, callback.ID, map[string]any{"process_status": "ignored", "error_code": "PAYMENT_NOT_FOUND", "processed_at": &now})
		}
		if err != nil {
			return err
		}
		orderRow, err := s.repo.LockOrder(ctx, tx, paymentLookup.OrderID)
		if err != nil {
			return err
		}
		payment, err := s.repo.LockPaymentByNo(ctx, tx, event.PaymentNo, providerCode)
		if err != nil {
			return err
		}
		if err := s.repo.UpdatePaymentCallback(ctx, tx, callback.ID, map[string]any{"payment_id": payment.ID}); err != nil {
			return err
		}
		state := ProviderPaymentState{ProviderTradeNo: event.ProviderTradeNo, PaymentNo: event.PaymentNo, Status: event.Status, AppID: event.AppID, MchID: event.MchID, Amount: event.Amount, Currency: event.Currency, AmountPresent: true, PaidAt: event.PaidAt}
		_, stateReject, err := s.applyProviderPaymentStateTx(ctx, tx, orderRow, payment, state, "system", 0, "callback:"+event.EventID)
		if err != nil {
			return err
		}
		now := time.Now()
		if stateReject != nil {
			callbackReject = stateReject
			return s.repo.UpdatePaymentCallback(ctx, tx, callback.ID, map[string]any{"process_status": "failed", "error_code": "PAYMENT_PROVIDER_DATA_MISMATCH", "processed_at": &now})
		}
		if strings.ToUpper(event.Status) != "SUCCESS" {
			return s.repo.UpdatePaymentCallback(ctx, tx, callback.ID, map[string]any{"process_status": "ignored", "processed_at": &now})
		}
		return s.repo.UpdatePaymentCallback(ctx, tx, callback.ID, map[string]any{"process_status": "processed", "processed_at": &now})
	})
	if err != nil {
		s.metrics.IncPayment(providerCode, "callback_failed")
		return err
	}
	if callbackReject != nil {
		s.metrics.IncPayment(providerCode, "callback_rejected")
		return callbackReject
	}
	s.metrics.IncPayment(providerCode, "callback_succeeded")
	return nil
}

// recordRejectedCallback 返回记录 Rejected 回调。
func (s *Service) recordRejectedCallback(ctx context.Context, providerCode string, eventID string, payloadHash string, errorCode string, signatureValid bool) error {
	now := time.Now()
	_, err := s.repo.CreatePaymentCallbackIfAbsent(ctx, s.repo.DB(), PaymentCallback{
		ID:              s.idGen.Next(),
		Provider:        providerCode,
		ProviderEventID: eventID,
		PayloadHash:     payloadHash,
		SignatureValid:  signatureValid,
		ProcessStatus:   "failed",
		ErrorCode:       stringPtr(errorCode),
		ReceivedAt:      now,
		ProcessedAt:     &now,
		RequestID:       requestctx.RequestIDPtr(ctx),
	})
	return err
}

// applyPaymentSuccess 应用支付成功状态。
func (s *Service) applyPaymentSuccess(ctx context.Context, tx *gorm.DB, row Order, payment Payment, event PaymentCallbackEvent, actorType string, actorID uint64, key string) error {
	items, err := s.repo.OrderItems(ctx, tx, row.ID)
	if err != nil {
		return err
	}
	for _, item := range items {
		stock, err := s.repo.LockStock(ctx, tx, item.ShopProductID)
		if err != nil {
			return err
		}
		if stock.ReservedQty < item.Quantity {
			return problem.Conflict("STOCK_RESERVATION_INCONSISTENT", "reserved stock not enough")
		}
		stockRecordID := s.idGen.Next()
		if err := s.repo.DeductReservedStock(ctx, tx, stock, item.Quantity); err != nil {
			return err
		}
		if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{
			ID: stockRecordID, ShopProductID: item.ShopProductID, ShopID: row.ShopID, ProductID: item.ProductID,
			ChangeType: "deduct", QuantityDelta: -item.Quantity, BeforeAvailableQty: stock.AvailableQty,
			AfterAvailableQty: stock.AvailableQty, SourceType: "payment", SourceID: payment.ID, IdempotencyKey: stringPtr(key),
		}); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, "stock.deducted", "stock_record", stockRecordID, map[string]any{"stock_record_id": idString(stockRecordID), "order_id": idString(row.ID), "payment_id": idString(payment.ID)}); err != nil {
			return err
		}
	}
	paidAt := event.PaidAt
	if paidAt == nil {
		now := time.Now()
		paidAt = &now
	}
	if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{
		"status": "succeeded", "provider_status": event.Status, "provider_trade_no": optionalString(event.ProviderTradeNo),
		"paid_at": paidAt, "failure_code": nil, "next_reconcile_at": nil, "version": gorm.Expr("version + 1"),
	}); err != nil {
		return err
	}
	if err := s.repo.UpdateOrder(ctx, tx, row.ID, map[string]any{
		"status": "paid", "pay_status": "succeeded", "paid_amount": payment.Amount, "paid_at": paidAt, "version": gorm.Expr("version + 1"),
	}); err != nil {
		return err
	}
	if s.dispatch != nil {
		if _, _, err := s.dispatch.EnsurePaidOrderTask(ctx, tx, dispatch.PaidOrderInput{OrderID: row.ID, ShopID: row.ShopID, AddressSnapshot: row.AddressSnapshot}); err != nil {
			return err
		}
	}
	if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{ID: s.idGen.Next(), OrderID: row.ID, ActorType: actorType, ActorID: actorID, Action: "pay", FromStatus: stringPtr(row.Status), ToStatus: stringPtr("paid"), RequestID: requestctx.RequestIDPtr(ctx)}); err != nil {
		return err
	}
	if err := s.createAudit(ctx, tx, actorType, actorID, "order.pay", "order", row.ID, row, map[string]any{"status": "paid", "payment_id": idString(payment.ID)}); err != nil {
		return err
	}
	return s.createOutbox(ctx, tx, "order.paid", "order", row.ID, map[string]any{"order_id": idString(row.ID), "payment_id": idString(payment.ID)})
}

// createOutbox 在状态变更的同一个事务里记录领域事件。
// RabbitMQ publisher 会在事务提交后读取这些行。
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

// createAudit 与订单状态变更处于同一个事务，保证审计和业务结果一致提交。
func (s *Service) createAudit(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, action string, resourceType string, resourceID uint64, before any, after any) error {
	return s.repo.CreateAuditLog(ctx, tx, AuditLog{
		ID:           s.idGen.Next(),
		ActorType:    actorType,
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

// aggregateItems 在库存校验前合并客户端传入的重复商品行。
func aggregateItems(items []OrderCreateItemReq) (map[uint64]int, error) {
	result := make(map[uint64]int)
	for _, item := range items {
		shopProductID, err := parseID(item.ShopProductID)
		if err != nil {
			return nil, problem.InvalidArgument("ORDER_EMPTY_ITEMS", "invalid shop_product_id")
		}
		if item.Quantity <= 0 || item.Quantity > 99 {
			return nil, problem.InvalidArgument("ORDER_EMPTY_ITEMS", "invalid quantity")
		}
		result[shopProductID] += item.Quantity
		if result[shopProductID] > 99 {
			return nil, problem.InvalidArgument("ORDER_EMPTY_ITEMS", "quantity exceeds limit")
		}
	}
	return result, nil
}

// customerIDFromClaims 从认证声明中解析并返回用户 ID。
func customerIDFromClaims(claims *auth.Claims) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
	}
	return parseID(claims.CustomerID)
}

type cancelActor struct {
	ActorID    uint64
	CustomerID uint64
	IsAdmin    bool
}

// cancelActorFromClaims 允许 C 端取消本人订单，也允许具备 order:cancel 的 admin 全局取消。
func cancelActorFromClaims(claims *auth.Claims) (cancelActor, error) {
	if claims == nil {
		return cancelActor{}, problem.Forbidden("PERM_FORBIDDEN", "authenticated account required")
	}
	switch claims.AccountType {
	case "customer":
		customerID, err := parseID(claims.CustomerID)
		if err != nil {
			return cancelActor{}, problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
		}
		return cancelActor{ActorID: customerID, CustomerID: customerID}, nil
	case "admin":
		adminID, err := parseID(claims.AdminUserID)
		if err != nil {
			return cancelActor{}, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
		}
		for _, permission := range claims.Permissions {
			if permission == "order:cancel" || permission == "order:cancel_all" {
				return cancelActor{ActorID: adminID, IsAdmin: true}, nil
			}
		}
		return cancelActor{}, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	default:
		return cancelActor{}, problem.Forbidden("PERM_FORBIDDEN", "customer or admin account required")
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

// orderNo 返回订单无。
func orderNo(orderID uint64) string {
	return "JXE" + strconv.FormatUint(orderID, 10)
}

// paymentNo 返回支付无。
func paymentNo(orderID uint64) string {
	return "PAY" + strconv.FormatUint(orderID, 10)
}

// idString 将数字 ID 转换为字符串。
func idString(id uint64) string {
	return strconv.FormatUint(id, 10)
}

// optionalString 将空字符串转换为空指针，否则返回字符串指针。
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// stringPtr 将非空字符串转换为字符串指针。
func stringPtr(value string) *string {
	return &value
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(value any) datatypes.JSON {
	payload, _ := json.Marshal(value)
	return datatypes.JSON(payload)
}

// addressSnapshot 返回地址快照。
func addressSnapshot(row CustomerAddress) map[string]any {
	return map[string]any{
		"contact_name":      row.ContactName,
		"contact_phone":     row.ContactPhone,
		"province":          row.Province,
		"city":              row.City,
		"city_code":         stringValue(row.CityCode),
		"district":          row.District,
		"district_code":     stringValue(row.DistrictCode),
		"address_detail":    row.AddressDetail,
		"doorplate":         stringValue(row.Doorplate),
		"poi_id":            stringValue(row.POIID),
		"formatted_address": stringValue(row.FormattedAddress),
		"latitude":          row.Latitude,
		"longitude":         row.Longitude,
		"coordinate_system": row.CoordinateSystem,
		"location_source":   row.LocationSource,
		"geocode_provider":  stringValue(row.GeocodeProvider),
		"geocode_status":    row.GeocodeStatus,
		"address_version":   row.Version,
	}
}

func contextContainsShop(value customerlocation.LocationContext, shopID string) bool {
	for _, candidate := range value.CandidateShops {
		if candidate.ID == shopID && candidate.Selectable {
			return true
		}
	}
	return false
}

func serviceShopChanged(value customerlocation.LocationContext) error {
	detail := problem.Conflict("SERVICE_SHOP_CHANGED", "requested shop is not serviceable for the current address")
	data := map[string]any{}
	if value.ServiceShop != nil {
		data["service_shop"] = value.ServiceShop
	}
	if value.DeliveryPromise != nil {
		data["delivery_promise"] = value.DeliveryPromise
	}
	detail.Data = data
	return detail
}

func pointerValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func routeProvider(source string) string {
	if source == "amap" || source == "cache" {
		return "amap"
	}
	return ""
}

// productSnapshot 返回商品快照。
func productSnapshot(row ShopProductRow) map[string]any {
	return map[string]any{
		"name":           row.Name,
		"brand_name":     stringValue(row.BrandName),
		"spec":           stringValue(row.Spec),
		"image_url":      stringValue(row.ImageURL),
		"age_restricted": row.AgeRestricted,
		"return_policy": map[string]any{
			"eligible":                row.ReturnEligible,
			"policy_code":             row.ReturnPolicyCode,
			"policy_version":          row.ReturnPolicyVersion,
			"sealed_package_required": row.SealedPackageRequired,
		},
	}
}

// orderDTO 返回订单DTO。
func orderDTO(row Order, items []OrderItem) OrderDTO {
	dto := OrderDTO{
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
	if row.ExpiresAt != nil {
		dto.ExpiresAt = row.ExpiresAt.Format(time.RFC3339)
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

// paymentDTO 返回支付DTO。
func paymentDTO(row Payment) PaymentDTO {
	paidAt := ""
	if row.PaidAt != nil {
		paidAt = row.PaidAt.Format(time.RFC3339)
	}
	expiresAt := ""
	if row.ExpiresAt != nil {
		expiresAt = row.ExpiresAt.Format(time.RFC3339)
	}
	clientPayload := map[string]any(nil)
	if len(row.ClientPayload) > 0 {
		_ = json.Unmarshal(row.ClientPayload, &clientPayload)
	}
	return PaymentDTO{
		ID:              idString(row.ID),
		PaymentNo:       row.PaymentNo,
		OrderID:         idString(row.OrderID),
		Channel:         row.Channel,
		Provider:        row.Provider,
		ProviderTradeNo: stringValue(row.ProviderTradeNo),
		Status:          row.Status,
		ProviderStatus:  stringValue(row.ProviderStatus),
		Amount:          row.Amount,
		Currency:        row.Currency,
		ClientPayload:   clientPayload,
		ExpiresAt:       expiresAt,
		PaidAt:          paidAt,
		RefundedAmount:  row.RefundedAmount,
	}
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
