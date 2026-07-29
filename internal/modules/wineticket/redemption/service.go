package redemption

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	LotSourcePurchase = core.LotSourcePurchase

	LotStatusActive   = core.LotStatusActive
	LotStatusDepleted = core.LotStatusDepleted
	LotStatusExpired  = core.LotStatusExpired
)

type RedemptionService struct {
	repo     *redemptionRepository
	core     *serviceCore
	ids      *snowflake.Generator
	dispatch RedemptionDispatchCoordinator
	now      func() time.Time
}

func NewRedemptionService(db *gorm.DB, ids *snowflake.Generator) *RedemptionService {
	return &RedemptionService{
		repo: newRedemptionRepository(db), core: newServiceCore(db, ids), ids: ids, now: time.Now,
	}
}

func (s *RedemptionService) WithDispatch(dispatch RedemptionDispatchCoordinator) *RedemptionService {
	s.dispatch = dispatch
	return s
}

func (s *RedemptionService) WithNow(now func() time.Time) *RedemptionService {
	if now != nil {
		s.now = now
		s.core.setClock(now)
	}
	return s
}

func (s *RedemptionService) nowShanghai() time.Time {
	return s.now().In(shanghaiLocation).Truncate(time.Millisecond)
}

func (s *RedemptionService) ParseDeliveryTimeSlotQuery(
	productIDRaw string,
	quantityRaw string,
	addressIDRaw string,
	addressVersionRaw string,
	dateFromRaw string,
	dateToRaw string,
) (RedemptionSlotQuery, error) {
	productID, err := parseExternalID(productIDRaw, "product_id")
	if err != nil {
		return RedemptionSlotQuery{}, err
	}
	addressID, err := parseExternalID(addressIDRaw, "address_id")
	if err != nil {
		return RedemptionSlotQuery{}, err
	}
	quantityValue, err := strconv.ParseUint(quantityRaw, 10, 16)
	if err != nil || quantityValue == 0 || quantityValue > uint64(redemptionQuantityMax) {
		return RedemptionSlotQuery{}, problem.InvalidArgument("VALIDATION_FAILED", "quantity must be between 1 and 1000")
	}
	addressVersionValue, err := strconv.ParseUint(addressVersionRaw, 10, 32)
	if err != nil || addressVersionValue == 0 {
		return RedemptionSlotQuery{}, problem.InvalidArgument("VALIDATION_FAILED", "address_version must be a positive integer")
	}

	now := s.nowShanghai()
	today, _ := time.ParseInLocation("2006-01-02", now.Format("2006-01-02"), shanghaiLocation)
	dateFrom := today
	dateTo := today.AddDate(0, 0, 7)
	if dateFromRaw != "" {
		dateFrom, err = time.ParseInLocation("2006-01-02", dateFromRaw, shanghaiLocation)
		if err != nil {
			return RedemptionSlotQuery{}, problem.InvalidArgument("VALIDATION_FAILED", "date_from must use YYYY-MM-DD")
		}
	}
	if dateToRaw != "" {
		dateTo, err = time.ParseInLocation("2006-01-02", dateToRaw, shanghaiLocation)
		if err != nil {
			return RedemptionSlotQuery{}, problem.InvalidArgument("VALIDATION_FAILED", "date_to must use YYYY-MM-DD")
		}
	}
	if dateFrom.Before(today) || dateTo.Before(dateFrom) || dateTo.After(dateFrom.AddDate(0, 0, 31)) {
		return RedemptionSlotQuery{}, problem.InvalidArgument("VALIDATION_FAILED", "date range must start today or later and span at most 31 days")
	}
	return RedemptionSlotQuery{
		ProductID: productID, Quantity: uint(quantityValue), AddressID: addressID,
		AddressVersion: uint(addressVersionValue), DateFrom: dateFrom, DateTo: dateTo,
	}, nil
}

func (s *RedemptionService) DeliveryTimeSlots(
	ctx context.Context,
	claims *auth.Claims,
	query RedemptionSlotQuery,
) ([]RedemptionDeliveryTimeSlotDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_slot:list")
	if err != nil {
		return nil, err
	}
	if query.ProductID == 0 || query.AddressID == 0 || query.AddressVersion == 0 ||
		query.Quantity == 0 || query.Quantity > redemptionQuantityMax ||
		query.DateFrom.IsZero() || query.DateTo.Before(query.DateFrom) {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery time slot query")
	}
	address, err := s.repo.address(ctx, customerID, query.AddressID, query.AddressVersion)
	if isRedemptionNotFound(err) {
		return nil, addressVersionConflict()
	}
	if err != nil {
		return nil, err
	}
	cityCode := redemptionAddressCityCode(address)
	if cityCode == "" {
		return nil, problem.Conflict("WT_NO_FULFILLABLE_SHOP", "address has no service city code")
	}

	now := s.nowShanghai()
	available, err := s.repo.totalAvailableByCityProduct(
		ctx, customerID, cityCode, query.ProductID, now, true,
	)
	if err != nil {
		return nil, err
	}
	if available < query.Quantity {
		withGuards, guardErr := s.repo.totalAvailableByCityProduct(
			ctx, customerID, cityCode, query.ProductID, now, false,
		)
		if guardErr != nil {
			return nil, guardErr
		}
		if withGuards >= query.Quantity {
			return nil, problem.Conflict("WT_RENEWAL_IN_PROGRESS", "a matching wine ticket lot is being renewed")
		}
		return nil, insufficientRedemptionQuantity()
	}

	rows, err := s.repo.listSlotRelations(ctx, query.ProductID, query.DateFrom, query.DateTo)
	if err != nil {
		return nil, err
	}
	items := make([]RedemptionDeliveryTimeSlotDTO, 0, len(rows))
	for _, row := range rows {
		if err := validateRedemptionRelation(row); err != nil {
			continue
		}
		startAt, endAt, err := redemptionSlotWindow(row.ServiceDate, row.StartTime, row.EndTime)
		if err != nil || !endAt.After(now) {
			continue
		}
		groupQuantity, err := s.repo.availableLotQuantity(
			ctx, customerID, row.MerchantID, cityCode, query.ProductID, endAt, true,
		)
		if err != nil {
			return nil, err
		}
		if groupQuantity < query.Quantity || row.StockAvailableQty < int(query.Quantity) {
			continue
		}
		items = append(items, redemptionSlotDTO(row, startAt, endAt, now))
	}
	if len(items) == 0 {
		return nil, problem.Conflict("WT_NO_FULFILLABLE_SHOP", "no shop can fulfill this wine ticket group and delivery window")
	}
	return items, nil
}

type normalizedRedemptionCreate struct {
	Request   RedemptionCreateRequest
	ProductID uint64
	AddressID uint64
	SlotID    uint64
}

func normalizeRedemptionCreate(req RedemptionCreateRequest) (normalizedRedemptionCreate, error) {
	req.ProductID = strings.TrimSpace(req.ProductID)
	req.AddressID = strings.TrimSpace(req.AddressID)
	req.DeliveryTimeSlotID = strings.TrimSpace(req.DeliveryTimeSlotID)
	productID, err := parseExternalID(req.ProductID, "product_id")
	if err != nil {
		return normalizedRedemptionCreate{}, err
	}
	addressID, err := parseExternalID(req.AddressID, "address_id")
	if err != nil {
		return normalizedRedemptionCreate{}, err
	}
	slotID, err := parseExternalID(req.DeliveryTimeSlotID, "delivery_time_slot_id")
	if err != nil {
		return normalizedRedemptionCreate{}, err
	}
	if req.Quantity == 0 || req.Quantity > redemptionQuantityMax {
		return normalizedRedemptionCreate{}, problem.InvalidArgument("VALIDATION_FAILED", "quantity must be between 1 and 1000")
	}
	if req.AddressVersion == 0 {
		return normalizedRedemptionCreate{}, problem.InvalidArgument("VALIDATION_FAILED", "address_version must be at least 1")
	}
	if req.Remark != nil {
		remark := strings.TrimSpace(*req.Remark)
		if !utf8.ValidString(remark) || utf8.RuneCountInString(remark) > redemptionRemarkRuneMax {
			return normalizedRedemptionCreate{}, problem.InvalidArgument("VALIDATION_FAILED", "remark must contain at most 200 characters")
		}
		if remark == "" {
			req.Remark = nil
		} else {
			req.Remark = &remark
		}
	}
	return normalizedRedemptionCreate{
		Request: req, ProductID: productID, AddressID: addressID, SlotID: slotID,
	}, nil
}

func (s *RedemptionService) Create(
	ctx context.Context,
	claims *auth.Claims,
	method string,
	path string,
	key string,
	req RedemptionCreateRequest,
) (response RedemptionDTO, resultErr error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_redemption:create")
	if err != nil {
		return RedemptionDTO{}, err
	}
	normalized, err := normalizeRedemptionCreate(req)
	if err != nil {
		return RedemptionDTO{}, err
	}
	if err := validateRedemptionIdempotencyKey(key); err != nil {
		return RedemptionDTO{}, err
	}
	requestHash := idempotency.RequestHash(normalized.Request)
	if replayed, err := s.core.idStore.ReplayCompleted(
		ctx, s.repo.dbConn(), claims.AccountType, customerID, path, key, requestHash, &response,
	); err != nil || replayed {
		return response, err
	}

	candidate, err := s.repo.resolveSlotRelation(ctx, s.repo.dbConn(), normalized.SlotID, normalized.ProductID)
	if isRedemptionNotFound(err) {
		return RedemptionDTO{}, problem.Conflict("WT_NO_FULFILLABLE_SHOP", "delivery slot has no eligible fulfillment relation")
	}
	if err != nil {
		return RedemptionDTO{}, err
	}
	candidateStart, candidateEnd, err := redemptionSlotWindow(candidate.ServiceDate, candidate.StartTime, candidate.EndTime)
	if err != nil {
		return RedemptionDTO{}, problem.Conflict("WT_NO_FULFILLABLE_SHOP", "delivery slot window is invalid")
	}

	resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.core.claimIdempotency(
			ctx, tx, claims.AccountType, customerID, method, path, key, requestHash,
		)
		if err != nil {
			return err
		}
		if !started {
			return s.core.cachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &response)
		}
		if s.dispatch == nil {
			return redemptionDispatchUnavailable()
		}

		now := s.nowShanghai()
		if err := s.requireAdultCustomer(ctx, tx, customerID, now); err != nil {
			return err
		}
		address, err := s.repo.lockAddress(
			ctx, tx, customerID, normalized.AddressID, normalized.Request.AddressVersion,
		)
		if isRedemptionNotFound(err) {
			return addressVersionConflict()
		}
		if err != nil {
			return err
		}
		cityCode := redemptionAddressCityCode(address)
		if cityCode == "" {
			return problem.Conflict("WT_NO_FULFILLABLE_SHOP", "address has no service city code")
		}
		addressSnapshot, err := marshalRedemptionAddressSnapshot(address)
		if err != nil {
			return err
		}

		lots, err := s.repo.lockEligibleLots(
			ctx, tx, customerID, candidate.MerchantID, cityCode,
			normalized.ProductID, candidateEnd,
		)
		if err != nil {
			return err
		}
		guardedLots, err := s.repo.lockActiveRenewalsForLots(ctx, tx, lots)
		if err != nil {
			return err
		}
		availableLots := redemptionLotsWithoutGuards(lots, guardedLots)
		if sumLotAvailability(availableLots) < normalized.Request.Quantity {
			if sumLotAvailability(lots) >= normalized.Request.Quantity {
				return problem.Conflict("WT_RENEWAL_IN_PROGRESS", "a matching wine ticket lot is being renewed")
			}
			return insufficientRedemptionQuantity()
		}
		lots = availableLots

		slot, err := s.repo.lockSlot(ctx, tx, normalized.SlotID)
		if isRedemptionNotFound(err) {
			return problem.Conflict("WT_SLOT_FULL", "delivery slot is no longer available")
		}
		if err != nil {
			return err
		}
		live, err := s.repo.resolveSlotRelation(ctx, tx, normalized.SlotID, normalized.ProductID)
		if isRedemptionNotFound(err) {
			return problem.Conflict("WT_NO_FULFILLABLE_SHOP", "fulfillment relation changed")
		}
		if err != nil {
			return err
		}
		if err := validateStableRedemptionRelation(candidate, live, slot); err != nil {
			return err
		}
		if err := validateRedemptionRelation(live); err != nil {
			return err
		}
		liveStart, liveEnd, err := redemptionSlotWindow(
			slot.ServiceDate,
			slot.StartTime,
			slot.EndTime,
		)
		if err != nil || !sameMillisecond(candidateStart, liveStart) || !sameMillisecond(candidateEnd, liveEnd) {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "delivery slot changed; refresh available slots")
		}
		if err := validateLockedRedemptionSlot(slot, liveStart, liveEnd, now); err != nil {
			return err
		}

		stock, err := s.repo.lockStock(ctx, tx, live.ShopProductID)
		if isRedemptionNotFound(err) {
			return stockNotEnough()
		}
		if err != nil {
			return err
		}
		if stock.ShopID != live.ShopID || stock.ProductID != normalized.ProductID ||
			stock.ShopProductID != live.ShopProductID || stock.AvailableQty < int(normalized.Request.Quantity) {
			return stockNotEnough()
		}

		redemptionID := s.ids.Next()
		orderID := s.ids.Next()
		redemptionNo := "WTRD" + idString(redemptionID)
		orderNo := "JXE" + idString(orderID)
		productSnapshot := marshalRedemptionProductSnapshot(live)
		slotSnapshot := marshalRedemptionSlotSnapshot(live, liveStart, liveEnd)
		notBeforeAt := liveStart.Add(-redemptionDispatchLead)
		if notBeforeAt.Before(now) {
			notBeforeAt = now
		}

		allocations, err := s.holdRedemptionLots(
			ctx, tx, customerID, redemptionID, redemptionNo,
			normalized.Request.Quantity, lots, now,
		)
		if err != nil {
			return err
		}
		if err := s.reserveRedemptionSlot(ctx, tx, slot, now); err != nil {
			return err
		}
		if err := s.deductRedemptionStock(
			ctx, tx, stock, normalized.Request.Quantity, redemptionID, now,
		); err != nil {
			return err
		}

		orderRow := order.Order{
			ID: orderID, OrderNo: orderNo, OrderType: redemptionOrderType,
			SettlementMode: redemptionSettlementMode, CustomerID: customerID,
			MerchantID: live.MerchantID, ShopID: live.ShopID, Status: "paid",
			PayStatus: redemptionPayStatus, DeliveryStatus: "pending_assign",
			GoodsAmount: 0, DiscountAmount: 0, DeliveryFeeAmount: 0,
			PayableAmount: 0, PaidAmount: 0, Remark: normalized.Request.Remark,
			AddressSnapshot:    cloneJSON(addressSnapshot),
			DeliveryTimeSlotID: &slot.ID, DeliveryTimeSlotSnapshot: cloneJSON(slotSnapshot),
			ComplianceSnapshot: jsonData(map[string]any{
				"schema_version": 1, "adult_realname_verified": true,
				"verified_at_action": "wine_ticket_redemption_create",
			}),
			IdempotencyKey: stringPointer(key), Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.createOrder(ctx, tx, &orderRow); err != nil {
			return err
		}
		orderItem := order.OrderItem{
			ID: s.ids.Next(), OrderID: orderID, ShopProductID: live.ShopProductID,
			ProductID: normalized.ProductID, ProductSnapshot: cloneJSON(productSnapshot),
			Quantity: int(normalized.Request.Quantity), SalePriceAmount: 0, TotalAmount: 0,
		}
		if err := s.repo.createOrderItem(ctx, tx, &orderItem); err != nil {
			return err
		}
		if err := s.repo.createOrderLog(ctx, tx, &order.OrderLog{
			ID: s.ids.Next(), OrderID: orderID, ActorType: "customer", ActorID: customerID,
			Action: "wine_ticket_redemption_create", ToStatus: stringPointer("paid"),
			RequestID: requestctx.RequestIDPtr(ctx),
		}); err != nil {
			return err
		}
		redemption := Redemption{
			ID: redemptionID, RedemptionNo: redemptionNo, CustomerID: customerID,
			IssuerMerchantID: live.MerchantID, ProductID: normalized.ProductID,
			ShopID: live.ShopID, ShopProductID: live.ShopProductID,
			DeliveryTimeSlotID: slot.ID, OrderID: orderID,
			Quantity: normalized.Request.Quantity, AddressID: address.ID,
			AddressVersion: address.Version, AddressSnapshot: cloneJSON(addressSnapshot),
			DeliveryTimeSlotSnapshot: cloneJSON(slotSnapshot),
			ProductSnapshot:          cloneJSON(productSnapshot), Status: RedemptionStatusScheduled,
			Version: 1, ScheduledStartAt: liveStart, ScheduledEndAt: liveEnd,
			NotBeforeAt: notBeforeAt, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.createRedemption(ctx, tx, &redemption); err != nil {
			return err
		}

		deliveryState, err := s.dispatch.EnsureRedemptionTaskTx(
			ctx, tx, RedemptionDispatchCreateInput{
				OrderID: orderID, ShopID: live.ShopID,
				AddressSnapshot:  cloneJSON(addressSnapshot),
				ScheduledStartAt: liveStart, ScheduledEndAt: liveEnd,
				NotBeforeAt: notBeforeAt,
			},
		)
		if err != nil {
			return err
		}
		if deliveryState.DeliveryOrderID == 0 || deliveryState.OrderID != orderID ||
			deliveryState.PickedUpAt != nil || deliveryState.CompletedAt != nil {
			return problem.Internal("dispatch returned an invalid wine ticket delivery state")
		}

		if allocationQuantity(allocations) != normalized.Request.Quantity {
			return problem.Internal("wine ticket redemption allocation total is inconsistent")
		}
		if err := s.core.createCustomerAudit(
			ctx, tx, customerID, "wine_ticket.redemption.create",
			"wine_ticket_redemption", redemptionID, nil,
			map[string]any{
				"redemption_no": redemptionNo, "order_no": orderNo,
				"quantity": normalized.Request.Quantity, "status": RedemptionStatusScheduled,
				"shop_id": idString(live.ShopID), "slot_id": idString(slot.ID),
			},
		); err != nil {
			return err
		}
		if err := s.core.createWineTicketOutbox(
			ctx, tx, "wine_ticket.redemption_created", "wine_ticket_redemption", redemptionID,
			map[string]any{
				"redemption_no": redemptionNo, "order_id": idString(orderID),
				"delivery_order_id": idString(deliveryState.DeliveryOrderID),
				"customer_id":       idString(customerID), "quantity": normalized.Request.Quantity,
				"scheduled_start_at": formatShanghai(liveStart),
				"scheduled_end_at":   formatShanghai(liveEnd),
				"not_before_at":      formatShanghai(notBeforeAt),
			},
		); err != nil {
			return err
		}
		response, err = s.redemptionDTOByID(ctx, tx, customerID, redemptionID)
		if err != nil {
			return err
		}
		return s.core.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, response)
	})
	if resultErr != nil {
		return RedemptionDTO{}, mapRedemptionDBError(resultErr)
	}
	return response, nil
}

func (s *RedemptionService) List(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	status string,
) ([]RedemptionDTO, string, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_redemption:list")
	if err != nil {
		return nil, "", err
	}
	status = strings.TrimSpace(status)
	if !validRedemptionStatus(status, true) {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid redemption status")
	}
	rows, err := s.repo.listCustomerRedemptions(ctx, customerID, query, status)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, idString(rows[len(rows)-1].ID))
	}
	items, err := s.redemptionDTOs(ctx, s.repo.dbConn(), rows)
	return items, next, err
}

func (s *RedemptionService) Detail(
	ctx context.Context,
	claims *auth.Claims,
	redemptionNo string,
) (RedemptionDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_redemption:view")
	if err != nil {
		return RedemptionDTO{}, err
	}
	redemptionNo = strings.TrimSpace(redemptionNo)
	if err := validateBusinessNo(redemptionNo, "redemption_no"); err != nil {
		return RedemptionDTO{}, err
	}
	row, err := s.repo.customerRedemptionByNo(ctx, customerID, redemptionNo)
	if isRedemptionNotFound(err) {
		return RedemptionDTO{}, redemptionNotFound()
	}
	if err != nil {
		return RedemptionDTO{}, err
	}
	items, err := s.redemptionDTOs(ctx, s.repo.dbConn(), []redemptionView{row})
	if err != nil {
		return RedemptionDTO{}, err
	}
	return items[0], nil
}

func (s *RedemptionService) Cancel(
	ctx context.Context,
	claims *auth.Claims,
	method string,
	path string,
	key string,
	redemptionNo string,
	req RedemptionCancelRequest,
) (response RedemptionDTO, resultErr error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_redemption:cancel")
	if err != nil {
		return RedemptionDTO{}, err
	}
	redemptionNo = strings.TrimSpace(redemptionNo)
	if err := validateBusinessNo(redemptionNo, "redemption_no"); err != nil {
		return RedemptionDTO{}, err
	}
	if req.ExpectedVersion == 0 {
		return RedemptionDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_version must be at least 1")
	}
	if err := validateRedemptionIdempotencyKey(key); err != nil {
		return RedemptionDTO{}, err
	}
	requestHash := idempotency.ResourceRequestHash("wine_ticket.redemption.cancel", redemptionNo, req)
	if replayed, err := s.core.idStore.ReplayCompleted(
		ctx, s.repo.dbConn(), claims.AccountType, customerID, path, key, requestHash, &response,
	); err != nil || replayed {
		return response, err
	}
	anchor, err := s.repo.cancellationAnchor(ctx, customerID, redemptionNo)
	if isRedemptionNotFound(err) {
		return RedemptionDTO{}, redemptionNotFound()
	}
	if err != nil {
		return RedemptionDTO{}, err
	}

	resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.core.claimIdempotency(
			ctx, tx, claims.AccountType, customerID, method, path, key, requestHash,
		)
		if err != nil {
			return err
		}
		if !started {
			return s.core.cachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &response)
		}
		if s.dispatch == nil {
			return redemptionDispatchUnavailable()
		}
		now := s.nowShanghai()
		if err := s.requireActiveCustomer(ctx, tx, customerID); err != nil {
			return err
		}

		// 调度协调器负责必需的 delivery_order 前缀锁。
		// 调用本方法前不得锁定订单、权益、配送时段或库存记录。
		delivery, err := s.dispatch.LockCancellationPrefixTx(ctx, tx, anchor.OrderID)
		if err != nil {
			return err
		}
		if delivery.DeliveryOrderID == 0 || delivery.OrderID != anchor.OrderID {
			return problem.Conflict("WT_REDEMPTION_NOT_CANCELLABLE", "delivery state is unavailable for cancellation")
		}
		orderRow, err := s.repo.lockOrder(ctx, tx, anchor.OrderID)
		if err != nil {
			return problem.Conflict("WT_REDEMPTION_NOT_CANCELLABLE", "redemption order is unavailable")
		}
		afterSaleIDs, err := s.repo.lockAfterSaleIDs(ctx, tx, orderRow.ID)
		if err != nil {
			return err
		}
		returnIDs, err := s.repo.lockDeliveryReturnIDs(ctx, tx, orderRow.ID)
		if err != nil {
			return err
		}
		if len(afterSaleIDs) != 0 || len(returnIDs) != 0 {
			return problem.Conflict("WT_REDEMPTION_NOT_CANCELLABLE", "redemption already entered after-sale or return handling")
		}
		redemption, err := s.repo.lockRedemption(ctx, tx, anchor.ID, customerID)
		if err != nil || redemption.OrderID != orderRow.ID {
			return problem.Conflict("WT_REDEMPTION_NOT_CANCELLABLE", "redemption relation is inconsistent")
		}
		if redemption.Status == RedemptionStatusCancelled {
			response, err = s.redemptionDTOByID(ctx, tx, customerID, redemption.ID)
			if err != nil {
				return err
			}
			return s.core.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, response)
		}
		if redemption.Version != req.ExpectedVersion {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "redemption changed; refresh and retry")
		}
		if err := validateCancellableRedemption(redemption, orderRow, delivery, customerID); err != nil {
			return err
		}

		allocations, err := s.repo.lockAllocations(ctx, tx, redemption.ID)
		if err != nil {
			return err
		}
		lots, err := s.repo.lockAllocationLots(ctx, tx, allocations)
		if err != nil {
			return err
		}
		if err := validateCancellationAllocations(redemption, allocations, lots); err != nil {
			return err
		}
		slot, err := s.repo.lockSlot(ctx, tx, redemption.DeliveryTimeSlotID)
		if err != nil || slot.ReservedOrders == 0 {
			return problem.Internal("wine ticket delivery slot reservation is inconsistent")
		}
		stock, err := s.repo.lockStock(ctx, tx, redemption.ShopProductID)
		if err != nil || stock.ShopID != redemption.ShopID || stock.ProductID != redemption.ProductID {
			return problem.Internal("wine ticket physical stock relation is inconsistent")
		}

		if err := s.restoreCancellationLots(
			ctx, tx, redemption, allocations, lots, now,
		); err != nil {
			return err
		}
		slotReleased, err := s.repo.releaseSlot(ctx, tx, slot, now)
		if err != nil {
			return err
		}
		if !slotReleased {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "delivery slot changed concurrently")
		}
		if err := s.restoreRedemptionStock(
			ctx, tx, stock, redemption.Quantity, redemption.ID, now,
		); err != nil {
			return err
		}

		cancelSource := "customer"
		cancelReason := "wine_ticket_redemption_cancelled"
		orderCancelled, err := s.repo.cancelOrder(
			ctx, tx, orderRow, cancelSource, cancelReason, now,
		)
		if err != nil {
			return err
		}
		if !orderCancelled {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "redemption order changed concurrently")
		}
		redemptionCancelled, err := s.repo.cancelRedemption(ctx, tx, redemption, now)
		if err != nil {
			return err
		}
		if !redemptionCancelled {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "redemption changed concurrently")
		}
		if err := s.dispatch.ApplyCancellationTx(
			ctx, tx, RedemptionDispatchCancelInput{
				State: delivery, CustomerID: customerID,
				ReasonCode: cancelReason, CancelledAt: now,
			},
		); err != nil {
			return err
		}
		if err := s.repo.createOrderLog(ctx, tx, &order.OrderLog{
			ID: s.ids.Next(), OrderID: orderRow.ID, ActorType: "customer", ActorID: customerID,
			Action: "wine_ticket_redemption_cancel", FromStatus: stringPointer(orderRow.Status),
			ToStatus: stringPointer("cancelled"), RequestID: requestctx.RequestIDPtr(ctx),
		}); err != nil {
			return err
		}
		if err := s.core.createCustomerAudit(
			ctx, tx, customerID, "wine_ticket.redemption.cancel",
			"wine_ticket_redemption", redemption.ID,
			map[string]any{
				"redemption_no": redemption.RedemptionNo,
				"status":        redemption.Status, "version": redemption.Version,
			},
			map[string]any{
				"redemption_no": redemption.RedemptionNo,
				"status":        RedemptionStatusCancelled, "version": redemption.Version + 1,
				"quantity": redemption.Quantity,
			},
		); err != nil {
			return err
		}
		if err := s.core.createWineTicketOutbox(
			ctx, tx, "wine_ticket.redemption_cancelled", "wine_ticket_redemption", redemption.ID,
			map[string]any{
				"redemption_no": redemption.RedemptionNo, "order_id": idString(orderRow.ID),
				"customer_id": idString(customerID), "quantity": redemption.Quantity,
				"restored_at": formatShanghai(now),
			},
		); err != nil {
			return err
		}
		response, err = s.redemptionDTOByID(ctx, tx, customerID, redemption.ID)
		if err != nil {
			return err
		}
		return s.core.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, response)
	})
	if resultErr != nil {
		return RedemptionDTO{}, mapRedemptionDBError(resultErr)
	}
	return response, nil
}

func (s *RedemptionService) holdRedemptionLots(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	redemptionID uint64,
	redemptionNo string,
	quantity uint,
	lots []core.Lot,
	now time.Time,
) ([]RedemptionAllocation, error) {
	remaining := quantity
	allocations := make([]RedemptionAllocation, 0, len(lots))
	assetRepo := core.NewTransactionAssetRepository(tx)
	for _, lot := range lots {
		if remaining == 0 {
			break
		}
		take := lot.AvailableQuantity
		if take > remaining {
			take = remaining
		}
		if take == 0 {
			continue
		}
		allocation := RedemptionAllocation{
			ID: s.ids.Next(), RedemptionID: redemptionID, LotID: lot.ID,
			Quantity: take, SourceExpiresAt: lot.ExpiresAt,
			Status: RedemptionAllocationStatusHeld, CreatedAt: now, UpdatedAt: now,
		}
		if _, err := s.core.assets.Redeem(ctx, assetRepo, core.AssetCommand{
			LotID:           lot.ID,
			OwnerCustomerID: customerID,
			Quantity:        take,
			TransactionType: TransactionTypeRedemptionHold,
			BizType:         "wine_ticket_redemption",
			BizID:           redemptionID,
			ActionKey:       fmt.Sprintf("redemption_hold:%d:%d", redemptionID, lot.ID),
			Metadata: map[string]any{
				"redemption_no": redemptionNo,
				"allocation_id": idString(allocation.ID),
			},
			OccurredAt: now,
		}); err != nil {
			return nil, err
		}
		if err := s.repo.createAllocation(ctx, tx, &allocation); err != nil {
			return nil, err
		}
		allocations = append(allocations, allocation)
		remaining -= take
	}
	if remaining != 0 {
		return nil, insufficientRedemptionQuantity()
	}
	return allocations, nil
}

func (s *RedemptionService) reserveRedemptionSlot(
	ctx context.Context,
	tx *gorm.DB,
	slot DeliveryTimeSlot,
	now time.Time,
) error {
	reserved, err := s.repo.reserveSlot(ctx, tx, slot, now)
	if err != nil {
		return err
	}
	if !reserved {
		return problem.Conflict("WT_SLOT_FULL", "delivery slot was filled concurrently")
	}
	return nil
}

func (s *RedemptionService) deductRedemptionStock(
	ctx context.Context,
	tx *gorm.DB,
	stock PhysicalStock,
	quantity uint,
	redemptionID uint64,
	now time.Time,
) error {
	if quantity == 0 || stock.AvailableQty < int(quantity) {
		return stockNotEnough()
	}
	afterAvailable := stock.AvailableQty - int(quantity)
	updated, err := s.repo.updatePhysicalStock(ctx, tx, stock, afterAvailable, now)
	if err != nil {
		return err
	}
	if !updated {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "physical stock changed concurrently")
	}
	beforeTotal := stock.AvailableQty + stock.ReservedQty + stock.LockedQty
	afterTotal := beforeTotal - int(quantity)
	actionKey := fmt.Sprintf("redemption_hold:%d:stock:%d", redemptionID, stock.ShopProductID)
	return s.repo.createStockRecord(ctx, tx, map[string]any{
		"id": s.ids.Next(), "shop_product_id": stock.ShopProductID,
		"shop_id": stock.ShopID, "product_id": stock.ProductID,
		"change_type": "wine_ticket_redeem", "quantity_delta": -int(quantity),
		"before_available_qty": stock.AvailableQty, "after_available_qty": afterAvailable,
		"total_quantity_delta": -int(quantity), "before_total_qty": beforeTotal,
		"after_total_qty": afterTotal, "source_type": "wine_ticket_redemption",
		"source_id": redemptionID, "idempotency_key": actionKey,
		"business_action_key": actionKey, "created_at": now, "updated_at": now,
	})
}

func (s *RedemptionService) restoreCancellationLots(
	ctx context.Context,
	tx *gorm.DB,
	redemption Redemption,
	allocations []RedemptionAllocation,
	lots []core.Lot,
	now time.Time,
) error {
	lotByID := make(map[uint64]core.Lot, len(lots))
	for _, lot := range lots {
		lotByID[lot.ID] = lot
	}
	assetRepo := core.NewTransactionAssetRepository(tx)
	for _, allocation := range allocations {
		lot, ok := lotByID[allocation.LotID]
		if !ok || allocation.Status != RedemptionAllocationStatusHeld ||
			allocation.Quantity == 0 || lot.AvailableQuantity > lot.TotalQuantity-allocation.Quantity {
			return problem.Internal("wine ticket redemption allocation cannot be restored safely")
		}
		mutation, err := s.core.assets.Restore(ctx, assetRepo, core.AssetCommand{
			LotID:           lot.ID,
			OwnerCustomerID: redemption.CustomerID,
			Quantity:        allocation.Quantity,
			TransactionType: TransactionTypeRedemptionRestore,
			BizType:         "wine_ticket_redemption",
			BizID:           redemption.ID,
			ActionKey:       fmt.Sprintf("redemption_restore:%d:%d", redemption.ID, lot.ID),
			Metadata: map[string]any{
				"redemption_no": redemption.RedemptionNo,
				"allocation_id": idString(allocation.ID),
			},
			OccurredAt: now,
			ExpiryEvidence: &core.AssetEvidence{
				TransactionType: TransactionTypeExpiry,
				BizType:         "wine_ticket_lot",
				BizID:           lot.ID,
				ActionKey: fmt.Sprintf(
					"expiry:%d:%d:after:redemption_restore:%d",
					lot.ID, lot.ExpiresAt.UnixMilli(), redemption.ID,
				),
				Metadata: map[string]any{
					"trigger":       "redemption_cancel",
					"redemption_no": redemption.RedemptionNo,
				},
			},
		})
		if err != nil {
			return err
		}
		lotByID[lot.ID] = mutation.Lot
		allocationRestored, err := s.repo.restoreAllocation(ctx, tx, allocation.ID, now)
		if err != nil {
			return err
		}
		if !allocationRestored {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "redemption allocation changed concurrently")
		}
	}
	return nil
}

func (s *RedemptionService) restoreRedemptionStock(
	ctx context.Context,
	tx *gorm.DB,
	stock PhysicalStock,
	quantity uint,
	redemptionID uint64,
	now time.Time,
) error {
	if quantity == 0 || stock.AvailableQty > int(^uint(0)>>1)-int(quantity) {
		return problem.Internal("physical stock restoration would overflow")
	}
	afterAvailable := stock.AvailableQty + int(quantity)
	updated, err := s.repo.updatePhysicalStock(ctx, tx, stock, afterAvailable, now)
	if err != nil {
		return err
	}
	if !updated {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "physical stock changed concurrently")
	}
	beforeTotal := stock.AvailableQty + stock.ReservedQty + stock.LockedQty
	afterTotal := beforeTotal + int(quantity)
	actionKey := fmt.Sprintf("redemption_restore:%d:stock:%d", redemptionID, stock.ShopProductID)
	return s.repo.createStockRecord(ctx, tx, map[string]any{
		"id": s.ids.Next(), "shop_product_id": stock.ShopProductID,
		"shop_id": stock.ShopID, "product_id": stock.ProductID,
		"change_type": "wine_ticket_redeem_restore", "quantity_delta": int(quantity),
		"before_available_qty": stock.AvailableQty, "after_available_qty": afterAvailable,
		"total_quantity_delta": int(quantity), "before_total_qty": beforeTotal,
		"after_total_qty": afterTotal, "source_type": "wine_ticket_redemption",
		"source_id": redemptionID, "idempotency_key": actionKey,
		"business_action_key": actionKey, "created_at": now, "updated_at": now,
	})
}

func validateCancellableRedemption(
	redemption Redemption,
	orderRow order.Order,
	delivery RedemptionDispatchState,
	customerID uint64,
) error {
	if redemption.CustomerID != customerID || orderRow.CustomerID != customerID ||
		redemption.OrderID != orderRow.ID || delivery.OrderID != orderRow.ID ||
		orderRow.OrderType != redemptionOrderType ||
		orderRow.SettlementMode != redemptionSettlementMode ||
		orderRow.PayStatus != redemptionPayStatus ||
		orderRow.GoodsAmount != 0 || orderRow.DiscountAmount != 0 ||
		orderRow.DeliveryFeeAmount != 0 || orderRow.PayableAmount != 0 ||
		orderRow.PaidAmount != 0 {
		return problem.Conflict("WT_REDEMPTION_NOT_CANCELLABLE", "redemption order settlement state is not cancellable")
	}
	if redemption.Status != RedemptionStatusScheduled && redemption.Status != RedemptionStatusAssigned {
		return problem.Conflict("WT_REDEMPTION_NOT_CANCELLABLE", "redemption can only be cancelled before pickup")
	}
	if redemption.PickedUpAt != nil || redemption.CompletedAt != nil ||
		delivery.PickedUpAt != nil || delivery.CompletedAt != nil {
		return problem.Conflict("WT_REDEMPTION_NOT_CANCELLABLE", "redemption has already been picked up")
	}
	switch delivery.Status {
	case "picked_up", "delivering", "completed", "delivered", "returning":
		return problem.Conflict("WT_REDEMPTION_NOT_CANCELLABLE", "delivery has already passed the cancellable state")
	}
	return nil
}

func validateCancellationAllocations(
	redemption Redemption,
	allocations []RedemptionAllocation,
	lots []core.Lot,
) error {
	if len(allocations) == 0 || allocationQuantity(allocations) != redemption.Quantity {
		return problem.Internal("wine ticket redemption allocation total is inconsistent")
	}
	lotByID := make(map[uint64]core.Lot, len(lots))
	for _, lot := range lots {
		lotByID[lot.ID] = lot
	}
	for _, allocation := range allocations {
		lot, ok := lotByID[allocation.LotID]
		if !ok || allocation.Status != RedemptionAllocationStatusHeld ||
			allocation.Quantity == 0 || lot.OwnerCustomerID != redemption.CustomerID ||
			lot.IssuerMerchantID != redemption.IssuerMerchantID ||
			lot.ProductID != redemption.ProductID {
			return problem.Internal("wine ticket redemption allocation relation is inconsistent")
		}
	}
	return nil
}

func validateStableRedemptionRelation(
	candidate redemptionSlotRelation,
	live redemptionSlotRelation,
	slot DeliveryTimeSlot,
) error {
	if candidate.SlotID != live.SlotID || live.SlotID != slot.ID ||
		candidate.ShopID != live.ShopID || live.ShopID != slot.ShopID ||
		candidate.MerchantID != live.MerchantID ||
		candidate.ShopProductID != live.ShopProductID ||
		candidate.ProductID != live.ProductID ||
		candidate.StockID != live.StockID {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "delivery slot fulfillment relation changed")
	}
	return nil
}

func redemptionLotsWithoutGuards(
	lots []core.Lot,
	guarded map[uint64]struct{},
) []core.Lot {
	if len(guarded) == 0 {
		return lots
	}
	available := make([]core.Lot, 0, len(lots)-len(guarded))
	for _, lot := range lots {
		if _, exists := guarded[lot.ID]; exists {
			continue
		}
		available = append(available, lot)
	}
	return available
}

func validateRedemptionRelation(row redemptionSlotRelation) error {
	if row.SlotID == 0 || row.ShopID == 0 || row.MerchantID == 0 ||
		row.ShopProductID == 0 || row.ProductID == 0 || row.StockID == 0 ||
		row.ShopMerchantID != row.MerchantID ||
		row.SPMerchantID != row.MerchantID ||
		row.SPShopID != row.ShopID || row.SPProductID != row.ProductID ||
		row.MerchantStatus != "active" || row.MerchantReview != "approved" ||
		row.ShopStatus != "active" || row.ShopBusinessStatus != "open" ||
		row.ShopProductStatus != "on_sale" || row.ProductStatus != "on_sale" ||
		row.CategoryStatus != "active" || !row.ProductAgeRestricted {
		return problem.Conflict("WT_NO_FULFILLABLE_SHOP", "shop, merchant, product, and stock relation is not eligible")
	}
	return nil
}

func validateLockedRedemptionSlot(
	slot DeliveryTimeSlot,
	startAt time.Time,
	endAt time.Time,
	now time.Time,
) error {
	if slot.Status != "open" || !startAt.Before(endAt) || !startAt.After(now) ||
		!slot.CutoffAt.After(now) {
		return problem.Conflict("WT_SLOT_FULL", "delivery slot is closed or past cutoff")
	}
	if slot.CapacityOrders == 0 || slot.ReservedOrders >= slot.CapacityOrders {
		return problem.Conflict("WT_SLOT_FULL", "delivery slot is full")
	}
	return nil
}

func (s *RedemptionService) requireAdultCustomer(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	now time.Time,
) error {
	if err := s.requireActiveCustomer(ctx, tx, customerID); err != nil {
		return err
	}
	current, err := s.repo.customerRealname(ctx, tx, customerID)
	if err == nil && current.Status == "verified" && current.AdultResult == "adult" &&
		current.RevokedAt == nil && (current.ExpiresAt == nil || current.ExpiresAt.After(now)) {
		return nil
	}
	if err != nil && !isRedemptionNotFound(err) {
		return err
	}
	pending, err := s.repo.pendingIdentityVerification(ctx, tx, customerID, now)
	if err != nil {
		return err
	}
	if pending {
		return problem.Conflict("IDENTITY_VERIFICATION_PENDING", "identity verification is still processing")
	}
	if current.Status == "verified" && current.AdultResult == "minor" && current.RevokedAt == nil {
		return problem.New(
			http.StatusUnprocessableEntity, "UNDERAGE_RESTRICTED",
			"Unprocessable Entity", "wine ticket redemption requires an adult customer",
		)
	}
	return problem.New(
		http.StatusUnprocessableEntity, "REALNAME_REQUIRED",
		"Unprocessable Entity", "valid real-name verification required",
	)
}

func (s *RedemptionService) requireActiveCustomer(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
) error {
	customer, err := s.repo.customerAccount(ctx, tx, customerID)
	if isRedemptionNotFound(err) || (err == nil && customer.Status != "active") {
		return problem.Forbidden("PERM_FORBIDDEN", "customer account is not active")
	}
	if err != nil {
		return err
	}
	return nil
}

func marshalRedemptionAddressSnapshot(address redemptionAddressRecord) (datatypes.JSON, error) {
	cityCode := redemptionAddressCityCode(address)
	districtCode := ""
	if address.DistrictCode != nil {
		districtCode = strings.TrimSpace(*address.DistrictCode)
	}
	payload, err := json.Marshal(redemptionAddressSnapshot{
		SchemaVersion: 1, AddressID: idString(address.ID), AddressVersion: address.Version,
		ContactName: address.ContactName, ContactPhone: address.ContactPhone,
		Province: address.Province, City: address.City, CityCode: cityCode,
		District: address.District, DistrictCode: districtCode,
		AddressDetail: address.AddressDetail, Doorplate: address.Doorplate,
		POIID: address.POIID, FormattedAddress: address.FormattedAddress,
		Latitude: address.Latitude, Longitude: address.Longitude,
		CoordinateSystem: address.CoordinateSystem, LocationSource: address.LocationSource,
		GeocodeProvider: address.GeocodeProvider, GeocodeStatus: address.GeocodeStatus,
	})
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(payload), nil
}

func marshalRedemptionProductSnapshot(row redemptionSlotRelation) datatypes.JSON {
	return jsonData(redemptionProductSnapshot{
		SchemaVersion: 1, ProductID: idString(row.ProductID), Name: row.ProductName,
		BrandName: row.ProductBrandName, Spec: row.ProductSpec, ImageURL: row.ProductImageURL,
	})
}

func marshalRedemptionSlotSnapshot(
	row redemptionSlotRelation,
	startAt time.Time,
	endAt time.Time,
) datatypes.JSON {
	return jsonData(redemptionSlotSnapshot{
		SchemaVersion: 1, SlotID: idString(row.SlotID), SlotVersion: row.SlotVersion,
		ShopID: idString(row.ShopID), ShopName: row.ShopName,
		IssuerMerchantID:          idString(row.MerchantID),
		IssuerMerchantDisplayName: row.MerchantName,
		ScheduledStartAt:          formatShanghai(startAt), ScheduledEndAt: formatShanghai(endAt),
		CutoffAt: formatShanghai(row.CutoffAt),
	})
}

func redemptionAddressCityCode(address redemptionAddressRecord) string {
	if address.CityCode == nil {
		return ""
	}
	return strings.TrimSpace(*address.CityCode)
}

func redemptionSlotWindow(
	serviceDate time.Time,
	startClock string,
	endClock string,
) (time.Time, time.Time, error) {
	date := serviceDate.Format("2006-01-02")
	startClock, err := normalizeRedemptionClock(startClock)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	endClock, err = normalizeRedemptionClock(endClock)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	startAt, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+startClock, shanghaiLocation)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	endAt, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+endClock, shanghaiLocation)
	if err != nil || !endAt.After(startAt) {
		return time.Time{}, time.Time{}, errors.New("invalid delivery slot window")
	}
	return startAt.Truncate(time.Millisecond), endAt.Truncate(time.Millisecond), nil
}

func normalizeRedemptionClock(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) >= 8 {
		for index := 0; index+8 <= len(value); index++ {
			candidate := value[index : index+8]
			if _, err := time.Parse("15:04:05", candidate); err == nil {
				return candidate, nil
			}
		}
	}
	return "", errors.New("invalid SQL clock")
}

func redemptionSlotDTO(
	row redemptionSlotRelation,
	startAt time.Time,
	endAt time.Time,
	now time.Time,
) RedemptionDeliveryTimeSlotDTO {
	availability := "open"
	switch {
	case row.SlotStatus != "open":
		availability = "closed"
	case !row.CutoffAt.After(now):
		availability = "cutoff"
	case row.ReservedOrders >= row.CapacityOrders:
		availability = "full"
	}
	remaining := uint(0)
	if row.CapacityOrders > row.ReservedOrders {
		remaining = row.CapacityOrders - row.ReservedOrders
	}
	return RedemptionDeliveryTimeSlotDTO{
		SlotID: idString(row.SlotID), ShopID: idString(row.ShopID), ShopName: row.ShopName,
		IssuerMerchantID:          idString(row.MerchantID),
		IssuerMerchantDisplayName: row.MerchantName,
		ScheduledStartAt:          formatShanghai(startAt), ScheduledEndAt: formatShanghai(endAt),
		CutoffAt: formatShanghai(row.CutoffAt), AvailabilityStatus: availability,
		RemainingCapacity: remaining, Version: row.SlotVersion,
	}
}

func sameMillisecond(left time.Time, right time.Time) bool {
	return left.Truncate(time.Millisecond).Equal(right.Truncate(time.Millisecond))
}

// SlotWindow、NormalizeClock 和 SameMillisecond 是迁移期间
// 与旧配送时段管理门面共享的窄兼容辅助方法。
func SlotWindow(
	serviceDate time.Time,
	startClock string,
	endClock string,
) (time.Time, time.Time, error) {
	return redemptionSlotWindow(serviceDate, startClock, endClock)
}

func NormalizeClock(value string) (string, error) {
	return normalizeRedemptionClock(value)
}

func SameMillisecond(left time.Time, right time.Time) bool {
	return sameMillisecond(left, right)
}

func ValidateLockedSlot(
	slot DeliveryTimeSlot,
	startAt time.Time,
	endAt time.Time,
	now time.Time,
) error {
	return validateLockedRedemptionSlot(slot, startAt, endAt, now)
}

func sumLotAvailability(lots []core.Lot) uint {
	var total uint
	for _, lot := range lots {
		if total > ^uint(0)-lot.AvailableQuantity {
			return ^uint(0)
		}
		total += lot.AvailableQuantity
	}
	return total
}

func allocationQuantity(allocations []RedemptionAllocation) uint {
	var total uint
	for _, allocation := range allocations {
		if total > ^uint(0)-allocation.Quantity {
			return ^uint(0)
		}
		total += allocation.Quantity
	}
	return total
}

func validRedemptionStatus(status string, allowEmpty bool) bool {
	if status == "" {
		return allowEmpty
	}
	switch status {
	case RedemptionStatusScheduled, RedemptionStatusAssigned, RedemptionStatusPickedUp,
		RedemptionStatusDelivered, RedemptionStatusCancelled,
		RedemptionStatusReturnInProgress, RedemptionStatusRestored, RedemptionStatusException:
		return true
	default:
		return false
	}
}

func validateRedemptionIdempotencyKey(key string) error {
	if key == "" {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
	}
	if len(key) < 8 || len(key) > 128 {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters")
	}
	return nil
}

func addressVersionConflict() error {
	return problem.Conflict("ADDRESS_VERSION_CONFLICT", "address was changed or is unavailable; refresh and reselect")
}

func insufficientRedemptionQuantity() error {
	return problem.Conflict("WT_INSUFFICIENT_QUANTITY", "insufficient wine ticket quantity in one issuer and city group")
}

func stockNotEnough() error {
	return problem.Conflict("STOCK_NOT_ENOUGH", "physical stock is not enough for this redemption")
}

func redemptionNotFound() error {
	return problem.NotFound("WT_REDEMPTION_NOT_FOUND", "wine ticket redemption not found")
}

func redemptionDispatchUnavailable() error {
	return problem.New(
		http.StatusServiceUnavailable, "WT_DISPATCH_UNAVAILABLE",
		"Service Unavailable", "atomic wine ticket dispatch coordinator is unavailable",
	)
}

func mapRedemptionDBError(err error) error {
	if err == nil {
		return nil
	}
	var details *problem.Details
	if errors.As(err, &details) {
		return details
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "deadlock") ||
		strings.Contains(message, "lock wait timeout") ||
		strings.Contains(message, "database is locked") ||
		strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "duplicate entry") {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket redemption changed concurrently")
	}
	return err
}

func sortedRedemptionIDs(rows []redemptionView) []uint64 {
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
