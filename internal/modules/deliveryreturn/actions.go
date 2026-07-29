package deliveryreturn

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

const maxHandoffAttempts = 5

type persistedActionResult struct {
	DTO         DTO    `json:"dto"`
	ErrorCode   string `json:"error_code,omitempty"`
	ErrorDetail string `json:"error_detail,omitempty"`
}

func (r persistedActionResult) err() error {
	if r.ErrorCode == "" {
		return nil
	}
	switch r.ErrorCode {
	case "HANDOFF_CODE_INVALID", "HANDOFF_CODE_EXPIRED":
		return problem.New(http.StatusUnprocessableEntity, r.ErrorCode, "Unprocessable Entity", r.ErrorDetail)
	case "MANUAL_REVIEW_REQUIRED":
		return problem.Conflict(r.ErrorCode, r.ErrorDetail)
	default:
		return problem.Conflict(r.ErrorCode, r.ErrorDetail)
	}
}

func (s *Service) Approve(ctx context.Context, claims *auth.Claims, method, route, key, idRaw string, req ApproveReq) (out DTO, resultErr error) {
	defer func() {
		s.auditFailure(ctx, claims, method+" "+route, "delivery_return.approve_denied", "delivery_return", idRaw, resultErr)
	}()
	if !s.cfg.DeliveryReturn.Enabled || !s.cfg.DeliveryReturn.ApprovalEnabled || !s.cfg.DeliveryReturn.SystemAfterSaleEnabled {
		return DTO{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DISABLED", "Service Unavailable", "delivery return approval is disabled")
	}
	actorID, err := requireAdmin(claims, "delivery_return:approve")
	if err != nil {
		return DTO{}, err
	}
	if s.afterSales == nil {
		return DTO{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DEPENDENCY_UNAVAILABLE", "Service Unavailable", "system after-sale service is unavailable")
	}
	id, err := parseID(idRaw)
	if err != nil {
		return DTO{}, returnNotFound()
	}
	if err := validateIdempotencyKey(key); err != nil {
		return DTO{}, err
	}
	note, err := cleanNote(req.DecisionNote)
	if err != nil || note == "" {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "decision_note is required")
	}
	hash := idempotency.RequestHash(map[string]any{"delivery_return_id": id, "body": req})
	var persisted persistedActionResult
	if replayed, replayErr := s.idem.ReplayCompleted(ctx, s.repo.DB(), "admin", actorID, route, key, hash, &persisted); replayErr != nil {
		return DTO{}, normalizeIdempotency(replayErr)
	} else if replayed {
		return persisted.DTO, persisted.err()
	}
	if err := s.checkRate(ctx, "approve:admin", actorID, time.Minute, 30); err != nil {
		return DTO{}, err
	}
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actorID, method, route, key, hash)
		if err != nil {
			return normalizeIdempotency(err)
		}
		if !started {
			return s.cachedAction(ctx, tx, "admin", actorID, route, key, &persisted)
		}
		ref, err := s.repo.ReturnByID(ctx, tx, id, false)
		if IsNotFound(err) {
			return returnNotFound()
		}
		if err != nil {
			return err
		}
		delivery, err := s.repo.LockDelivery(ctx, tx, ref.DeliveryOrderID)
		if err != nil {
			return returnNotFound()
		}
		order, err := s.repo.LockOrder(ctx, tx, ref.OrderID)
		if err != nil {
			return problem.Conflict("INVALID_RETURN_STATE", "delivery return order is unavailable")
		}
		handler, err := s.settlementHandler(order)
		if err != nil {
			return err
		}
		row, err := s.repo.ReturnByID(ctx, tx, id, true)
		if err != nil {
			return returnNotFound()
		}
		if err := validateReturnSettlementRoute(handler, row); err != nil {
			return err
		}
		if row.Version != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "delivery return version changed")
		}
		if row.Status != StatusRequested || delivery.Status != "delivering" || delivery.PickedUpAt == nil {
			return problem.Conflict("INVALID_RETURN_STATE", "delivery return cannot be approved in its current state")
		}
		if err := s.approveReturnLocked(ctx, tx, &row, delivery, order, handler, actorID, note); err != nil {
			if errors.Is(err, aftersale.ErrDeliveryReturnManualReview) {
				if transitionErr := s.markManualReview(ctx, tx, &row, actorID, key, "customer after-sale or refund reservation conflicts with full refund"); transitionErr != nil {
					return transitionErr
				}
				aggregate, loadErr := s.repo.AggregateTx(ctx, tx, row.ID)
				if loadErr != nil {
					return loadErr
				}
				persisted = persistedActionResult{DTO: s.dto(aggregate, "admin"), ErrorCode: "MANUAL_REVIEW_REQUIRED", ErrorDetail: "existing after-sale or refund reservation requires manual review"}
				return s.idem.Succeed(ctx, tx, "admin", actorID, route, key, persisted)
			}
			return err
		}
		aggregate, err := s.repo.AggregateTx(ctx, tx, row.ID)
		if err != nil {
			return err
		}
		persisted = persistedActionResult{DTO: s.dto(aggregate, "admin")}
		return s.idem.Succeed(ctx, tx, "admin", actorID, route, key, persisted)
	})
	if err != nil {
		return DTO{}, err
	}
	return persisted.DTO, persisted.err()
}

func (s *Service) approveReturnLocked(
	ctx context.Context,
	tx *gorm.DB,
	row *Return,
	delivery DeliveryOrder,
	order OrderRef,
	handler ReturnSettlementHandler,
	actorID uint64,
	note string,
) error {
	result, err := handler.Approve(ctx, tx, *row, delivery, order, actorID, note)
	if err != nil {
		return err
	}
	if result.AfterSaleID == 0 || result.SettlementBizID == nil || *result.SettlementBizID == 0 {
		return problem.Internal("delivery return settlement approval is incomplete")
	}
	if result.OrderStatus == "" || result.AfterSaleStatus == "" {
		return problem.Internal("delivery return settlement order projection is incomplete")
	}
	now := s.now().UTC()
	deadline := now.Add(s.cfg.DeliveryReturn.ReceiptDeadlineAfter)
	from := row.Status
	updated, err := s.repo.UpdateReturnVersioned(ctx, tx, *row, map[string]any{
		"status": StatusReturning, "after_sale_id": result.AfterSaleID,
		"settlement_type": handler.SettlementType(), "settlement_biz_id": *result.SettlementBizID,
		"settlement_status": "processing",
		"approved_by":       actorID, "approved_at": now, "receipt_deadline_at": deadline,
	})
	if err != nil {
		return err
	}
	if !updated {
		return problem.Conflict("VERSION_CONFLICT", "delivery return version changed")
	}
	if err := s.repo.UpdateDelivery(ctx, tx, delivery.ID, map[string]any{
		"status": "returning", "assignment_version": gorm.Expr("assignment_version+1"),
	}); err != nil {
		return err
	}
	// 批准会结束原始末端履约。任何尚未验证的送达码，
	// 在逆向物流期间都绝不能继续使用。
	if err := deliveryverification.Invalidate(ctx, tx, s.ids, delivery.ID, "delivery_return_approved"); err != nil {
		return err
	}
	if err := s.repo.UpdateOrder(ctx, tx, row.OrderID, map[string]any{
		"status": result.OrderStatus, "delivery_status": "returning",
		"after_sale_status": result.AfterSaleStatus, "version": gorm.Expr("version+1"),
	}); err != nil {
		return err
	}
	row.Status, row.AfterSaleID, row.ApprovedBy, row.ApprovedAt, row.ReceiptDeadlineAt = StatusReturning, &result.AfterSaleID, &actorID, &now, &deadline
	row.SettlementType, row.SettlementBizID, row.SettlementStatus = stringPointer(handler.SettlementType()), result.SettlementBizID, stringPointer("processing")
	row.Version++
	return s.writeFacts(ctx, tx, *row, "admin", &actorID, "approve", from, StatusReturning, "")
}

func (s *Service) markManualReview(ctx context.Context, tx *gorm.DB, row *Return, actorID uint64, key, detail string) error {
	from := row.Status
	// 该冲突在审批创建结算业务记录前发现。
	// 独立结算轴应保持 not_started：
	// 零售 Contract 记录只有绑定 settlement_biz_id 后才能进入异常状态。
	updated, err := s.repo.UpdateReturnVersioned(ctx, tx, *row, map[string]any{"status": StatusDisputed})
	if err != nil || !updated {
		if err != nil {
			return err
		}
		return problem.Conflict("VERSION_CONFLICT", "delivery return version changed")
	}
	row.Status, row.Version = StatusDisputed, row.Version+1
	if err := s.writeFacts(ctx, tx, *row, "admin", &actorID, "manual_review", from, StatusDisputed, key); err != nil {
		return err
	}
	return s.repo.CreateHistory(ctx, tx, History{
		ID: s.ids.Next(), DeliveryReturnID: row.ID, FromStatus: optionalString(StatusDisputed), ToStatus: optionalString(StatusDisputed),
		Action: "manual_review_reason", ActorType: "admin", ActorID: &actorID,
		RequestID: requestctx.RequestIDPtr(ctx), MetadataJSON: jsonData(map[string]any{"detail": detail}), CreatedAt: s.now().UTC(),
	})
}

func (s *Service) Arrive(ctx context.Context, claims *auth.Claims, method, route, key, idRaw string, req ArriveReq) (out DTO, resultErr error) {
	defer func() {
		s.auditFailure(ctx, claims, method+" "+route, "delivery_return.arrive_denied", "delivery_return", idRaw, resultErr)
	}()
	if !s.cfg.DeliveryReturn.Enabled || !s.cfg.DeliveryReturn.RiderWriteEnabled {
		return DTO{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DISABLED", "Service Unavailable", "delivery return arrival is disabled")
	}
	riderID, err := requireRider(claims, "delivery_return:arrive")
	if err != nil {
		return DTO{}, err
	}
	if !allowlisted(s.cfg.DeliveryReturn.RiderAllowlist, riderID) {
		return DTO{}, problem.Forbidden("RETURN_FORBIDDEN", "rider is outside the delivery return rollout")
	}
	id, err := parseID(idRaw)
	if err != nil {
		return DTO{}, returnNotFound()
	}
	if err := validateIdempotencyKey(key); err != nil {
		return DTO{}, err
	}
	hash := idempotency.RequestHash(map[string]any{"delivery_return_id": id, "body": req})
	var persisted persistedActionResult
	if replayed, replayErr := s.idem.ReplayCompleted(ctx, s.repo.DB(), "rider", riderID, route, key, hash, &persisted); replayErr != nil {
		return DTO{}, normalizeIdempotency(replayErr)
	} else if replayed {
		return persisted.DTO, persisted.err()
	}
	if err := s.checkRate(ctx, "arrive:rider", riderID, 10*time.Minute, 20); err != nil {
		return DTO{}, err
	}
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "rider", riderID, method, route, key, hash)
		if err != nil {
			return normalizeIdempotency(err)
		}
		if !started {
			return s.cachedAction(ctx, tx, "rider", riderID, route, key, &persisted)
		}
		ref, err := s.repo.ReturnByID(ctx, tx, id, false)
		if err != nil || ref.RiderID != riderID {
			return returnNotFound()
		}
		delivery, err := s.repo.LockDelivery(ctx, tx, ref.DeliveryOrderID)
		if err != nil {
			return returnNotFound()
		}
		row, err := s.repo.ReturnByID(ctx, tx, id, true)
		if err != nil || row.RiderID != riderID {
			return returnNotFound()
		}
		if row.Version != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "delivery return version changed")
		}
		reissue := row.Status == StatusArrived && row.HandoffCodeExpiresAt != nil && (s.now().After(*row.HandoffCodeExpiresAt) || row.HandoffFailedAttempts >= maxHandoffAttempts)
		if (row.Status != StatusReturning && !reissue) || delivery.Status != "returning" {
			return problem.Conflict("INVALID_RETURN_STATE", "delivery return cannot arrive in its current state")
		}
		code, err := newHandoffCode()
		if err != nil {
			return err
		}
		hashBytes, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		expires := now.Add(s.cfg.DeliveryReturn.HandoffTTL)
		from := row.Status
		values := map[string]any{
			"status": StatusArrived, "handoff_code_hash": string(hashBytes),
			"handoff_code_expires_at": expires, "handoff_failed_attempts": 0,
		}
		if row.ArrivedAt == nil {
			values["arrived_at"] = now
		}
		updated, err := s.repo.UpdateReturnVersioned(ctx, tx, row, values)
		if err != nil || !updated {
			if err != nil {
				return err
			}
			return problem.Conflict("VERSION_CONFLICT", "delivery return version changed")
		}
		row.Status, row.Version, row.HandoffCodeHash, row.HandoffCodeExpiresAt, row.HandoffFailedAttempts = StatusArrived, row.Version+1, stringPointer(string(hashBytes)), &expires, 0
		if row.ArrivedAt == nil {
			row.ArrivedAt = &now
		}
		if err := s.writeFacts(ctx, tx, row, "rider", &riderID, "arrive", from, StatusArrived, key); err != nil {
			return err
		}
		aggregate, err := s.repo.AggregateTx(ctx, tx, row.ID)
		if err != nil {
			return err
		}
		dto := s.dto(aggregate, "rider")
		dto.HandoffCode, dto.HandoffCodeExpiresAt = code, timeString(expires)
		persisted = persistedActionResult{DTO: dto}
		return s.idem.Succeed(ctx, tx, "rider", riderID, route, key, persisted)
	})
	if err != nil {
		return DTO{}, err
	}
	return persisted.DTO, persisted.err()
}

func (s *Service) Receive(ctx context.Context, claims *auth.Claims, method, route, key, idRaw string, req ReceiveReq) (out DTO, resultErr error) {
	defer func() {
		s.auditFailure(ctx, claims, method+" "+route, "delivery_return.receive_denied", "delivery_return", idRaw, resultErr)
	}()
	if !s.cfg.DeliveryReturn.Enabled || !s.cfg.DeliveryReturn.ReceiptEnabled {
		return DTO{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DISABLED", "Service Unavailable", "delivery return receipt is disabled")
	}
	actorID, shopIDs, err := requireStore(claims, "delivery_return:receive_shop")
	if err != nil {
		return DTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return DTO{}, returnNotFound()
	}
	if err := validateIdempotencyKey(key); err != nil {
		return DTO{}, err
	}
	if err := validateReceiveRequest(&req); err != nil {
		return DTO{}, err
	}
	hash := idempotency.RequestHash(map[string]any{"delivery_return_id": id, "body": req})
	var persisted persistedActionResult
	if replayed, replayErr := s.idem.ReplayCompleted(ctx, s.repo.DB(), "merchant", actorID, route, key, hash, &persisted); replayErr != nil {
		return DTO{}, normalizeIdempotency(replayErr)
	} else if replayed {
		return persisted.DTO, persisted.err()
	}
	if err := s.checkRate(ctx, "receive:merchant", actorID, time.Minute, 60); err != nil {
		return DTO{}, err
	}
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "merchant", actorID, method, route, key, hash)
		if err != nil {
			return normalizeIdempotency(err)
		}
		if !started {
			return s.cachedAction(ctx, tx, "merchant", actorID, route, key, &persisted)
		}
		ref, err := s.repo.ReturnByID(ctx, tx, id, false)
		if err != nil || !containsID(shopIDs, ref.ShopID) {
			return returnNotFound()
		}
		if !allowlisted(s.cfg.DeliveryReturn.ShopAllowlist, ref.ShopID) {
			return problem.Forbidden("RETURN_FORBIDDEN", "shop is outside the delivery return rollout")
		}
		if ref.AfterSaleID == nil {
			return problem.Conflict("INVALID_RETURN_STATE", "delivery return system after-sale is missing")
		}
		delivery, err := s.repo.LockDelivery(ctx, tx, ref.DeliveryOrderID)
		if err != nil {
			return returnNotFound()
		}
		order, err := s.repo.LockOrder(ctx, tx, ref.OrderID)
		if err != nil {
			return problem.Conflict("INVALID_RETURN_STATE", "delivery return order is unavailable")
		}
		handler, err := s.settlementHandler(order)
		if err != nil {
			return err
		}
		// 退款回调会在退回关闭钩子前锁定售后单。
		// 此处保持相同的相对顺序，避免回调与收货之间形成循环等待。
		afterSale, err := s.repo.AfterSale(ctx, tx, *ref.AfterSaleID, true)
		if err != nil {
			return problem.Conflict("INVALID_RETURN_STATE", "system after-sale link is invalid")
		}
		row, err := s.repo.ReturnByID(ctx, tx, id, true)
		if err != nil || !containsID(shopIDs, row.ShopID) {
			return returnNotFound()
		}
		if row.Version != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "delivery return version changed")
		}
		if err := validateReturnSettlementRoute(handler, row); err != nil {
			return err
		}
		if row.Status != StatusArrived && row.Status != StatusException {
			return problem.Conflict("INVALID_RETURN_STATE", "delivery return is not ready for receipt")
		}
		if row.AfterSaleID == nil || *row.AfterSaleID != afterSale.ID || row.HandoffCodeHash == nil || row.HandoffCodeExpiresAt == nil {
			return problem.Conflict("INVALID_RETURN_STATE", "delivery return handoff is incomplete")
		}
		if s.now().After(*row.HandoffCodeExpiresAt) {
			persisted = persistedActionResult{ErrorCode: "HANDOFF_CODE_EXPIRED", ErrorDetail: "handoff code has expired"}
			return s.persistRejectedHandoff(ctx, tx, &row, actorID, route, key, persisted, "expired")
		}
		if row.HandoffFailedAttempts >= maxHandoffAttempts || bcrypt.CompareHashAndPassword([]byte(*row.HandoffCodeHash), []byte(req.HandoffCode)) != nil {
			persisted = persistedActionResult{ErrorCode: "HANDOFF_CODE_INVALID", ErrorDetail: "handoff code is invalid"}
			return s.persistRejectedHandoff(ctx, tx, &row, actorID, route, key, persisted, "invalid")
		}
		if afterSale.SourceType != "delivery_return" || afterSale.SourceID == nil || *afterSale.SourceID != row.ID {
			return problem.Conflict("INVALID_RETURN_STATE", "system after-sale link is invalid")
		}
		afterItems, err := s.repo.AfterSaleItems(ctx, tx, afterSale.ID)
		if err != nil {
			return err
		}
		if len(afterItems) != len(req.Items) {
			return s.persistQuantityDispute(ctx, tx, &row, actorID, route, key, &persisted)
		}
		requested := make(map[uint64]ReceiveItemReq, len(req.Items))
		for _, input := range req.Items {
			itemID, _ := parseID(input.AfterSaleItemID)
			requested[itemID] = input
		}
		orderItemIDs := make([]uint64, 0, len(afterItems))
		for _, item := range afterItems {
			input, ok := requested[item.ID]
			if !ok || input.ReceivedQuantity != item.ApprovedQuantity {
				return s.persistQuantityDispute(ctx, tx, &row, actorID, route, key, &persisted)
			}
			orderItemIDs = append(orderItemIDs, item.OrderItemID)
		}
		snapshots, err := s.repo.OrderItemSnapshots(ctx, tx, orderItemIDs)
		if err != nil {
			return err
		}
		receiptID := s.ids.Next()
		receiptItems := make([]ReceiptItem, 0, len(afterItems))
		restockIDs := make([]uint64, 0)
		for _, item := range afterItems {
			input := requested[item.ID]
			policyCode, policyVersion, eligible := returnPolicy(snapshots[item.OrderItemID])
			if input.Disposition == "restock" && !eligible {
				return problem.Conflict("INVALID_DISPOSITION", "item return policy does not allow restock")
			}
			if input.Disposition == "restock" {
				restockIDs = append(restockIDs, item.ShopProductID)
			}
			receiptItems = append(receiptItems, ReceiptItem{
				ID: s.ids.Next(), ReturnReceiptID: receiptID, AfterSaleItemID: item.ID,
				OrderItemID: item.OrderItemID, ShopProductID: item.ShopProductID, ProductID: item.ProductID,
				ExpectedQuantity: item.ApprovedQuantity, ReceivedQuantity: input.ReceivedQuantity,
				Disposition: input.Disposition, PolicyCode: policyCode, PolicyVersion: policyVersion,
				Note: optionalString(input.Note),
			})
		}
		settlementPlan, stocks, err := s.prepareReceivedSettlementAndLockStocks(
			ctx,
			tx,
			row,
			afterSale,
			order,
			handler,
			restockIDs,
		)
		if err != nil {
			return err
		}
		available := map[uint64]int{}
		totals := map[uint64]int{}
		for id, stock := range stocks {
			available[id] = stock.AvailableQty
			totals[id] = stock.AvailableQty + stock.ReservedQty + stock.LockedQty
		}
		for index := range receiptItems {
			item := &receiptItems[index]
			if item.Disposition != "restock" {
				continue
			}
			stock, ok := stocks[item.ShopProductID]
			if !ok {
				return problem.Conflict("STOCK_NOT_FOUND", "return stock row not found")
			}
			before := available[item.ShopProductID]
			beforeTotal := totals[item.ShopProductID]
			if err := s.repo.AddAvailable(ctx, tx, stock, item.ReceivedQuantity); err != nil {
				return err
			}
			after := before + item.ReceivedQuantity
			available[item.ShopProductID] = after
			totals[item.ShopProductID] = beforeTotal + item.ReceivedQuantity
			item.AvailableBefore, item.AvailableAfter = &before, &after
			businessKey := fmt.Sprintf("delivery_return:%d:%d:restock", row.ID, item.AfterSaleItemID)
			if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{
				ID: s.ids.Next(), ShopProductID: item.ShopProductID, ShopID: row.ShopID, ProductID: item.ProductID,
				ChangeType: "return", QuantityDelta: item.ReceivedQuantity, BeforeAvailableQty: before, AfterAvailableQty: after,
				TotalQuantityDelta: item.ReceivedQuantity, BeforeTotalQty: beforeTotal, AfterTotalQty: beforeTotal + item.ReceivedQuantity,
				SourceType: "delivery_return", SourceID: &row.ID, IdempotencyKey: optionalString(key), BusinessActionKey: &businessKey,
			}); err != nil {
				return err
			}
		}
		disposition := aggregateDisposition(receiptItems)
		now := s.now().UTC()
		receipt := ReturnReceipt{
			ID: receiptID, ReceiptNo: "RR" + idString(receiptID), AfterSaleID: afterSale.ID,
			ShopID: row.ShopID, Disposition: disposition, SealedPackageIntact: disposition == "restock",
			GoodsIntact: disposition != "damaged", ReceivedBy: actorID, ReceivedAt: now,
		}
		if err := s.repo.CreateReceipt(ctx, tx, receipt, receiptItems); err != nil {
			return err
		}
		for _, item := range receiptItems {
			if err := s.repo.UpdateAfterSaleItemDisposition(ctx, tx, item.AfterSaleItemID, item.Disposition); err != nil {
				return err
			}
		}
		from := row.Status
		updated, err := s.repo.UpdateReturnVersioned(ctx, tx, row, map[string]any{"status": StatusReceived, "received_at": now})
		if err != nil || !updated {
			if err != nil {
				return err
			}
			return problem.Conflict("VERSION_CONFLICT", "delivery return version changed")
		}
		if err := s.repo.UpdateDelivery(ctx, tx, delivery.ID, map[string]any{"status": "returned", "assignment_version": gorm.Expr("assignment_version+1")}); err != nil {
			return err
		}
		if err := deliveryverification.Invalidate(ctx, tx, s.ids, delivery.ID, "delivery_return_received"); err != nil {
			return err
		}
		if err := s.repo.UpdateOrder(ctx, tx, row.OrderID, map[string]any{"delivery_status": "returned", "version": gorm.Expr("version+1")}); err != nil {
			return err
		}
		row.Status, row.ReceivedAt, row.Version = StatusReceived, &now, row.Version+1
		if err := s.writeFacts(ctx, tx, row, "merchant", &actorID, "receive", from, StatusReceived, key); err != nil {
			return err
		}
		if err := s.trySettleReceivedLocked(
			ctx,
			tx,
			&row,
			afterSale,
			order,
			handler,
			settlementPlan,
		); err != nil {
			return err
		}
		aggregate, err := s.repo.AggregateTx(ctx, tx, row.ID)
		if err != nil {
			return err
		}
		persisted = persistedActionResult{DTO: s.dto(aggregate, "store")}
		return s.idem.Succeed(ctx, tx, "merchant", actorID, route, key, persisted)
	})
	if err != nil {
		return DTO{}, err
	}
	// 收货事务可能在并发退款回调提交前已经开始。使用新事务重新计算关闭状态，
	// 避免 MySQL REPEATABLE READ 让已完全结束的退回滞留在 received 状态。
	if persisted.ErrorCode == "" &&
		persisted.DTO.SettlementType == SettlementRetailCashRefund &&
		persisted.DTO.AfterSaleID != "" {
		if afterSaleID, parseErr := parseID(persisted.DTO.AfterSaleID); parseErr == nil {
			_ = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				return s.TryCloseByAfterSaleWithTx(ctx, tx, afterSaleID)
			})
		}
	}
	return persisted.DTO, persisted.err()
}

func (s *Service) prepareReceivedSettlementAndLockStocks(
	ctx context.Context,
	tx *gorm.DB,
	row Return,
	afterSale AfterSale,
	order OrderRef,
	handler ReturnSettlementHandler,
	restockIDs []uint64,
) (ReturnSettlementReceivePlan, map[uint64]ProductStock, error) {
	plan, err := prepareReturnSettlementReceived(ctx, tx, handler, row, afterSale, order)
	if err != nil {
		return nil, nil, err
	}
	stocks := map[uint64]ProductStock{}
	if len(restockIDs) == 0 {
		return plan, stocks, nil
	}
	stocks, err = s.repo.LockStocks(ctx, tx, uniqueIDs(restockIDs))
	if err != nil {
		return nil, nil, err
	}
	return plan, stocks, nil
}

func (s *Service) trySettleReceivedLocked(
	ctx context.Context,
	tx *gorm.DB,
	row *Return,
	afterSale AfterSale,
	order OrderRef,
	handler ReturnSettlementHandler,
	settlementPlan ReturnSettlementReceivePlan,
) error {
	if row.Status != StatusReceived && row.Status != StatusException {
		return nil
	}
	complete, err := s.repo.ClosureComplete(ctx, tx, afterSale.ID, row.ID)
	if err != nil || !complete {
		return err
	}
	ready, err := applyReturnSettlementReceived(
		ctx,
		tx,
		handler,
		settlementPlan,
		*row,
		afterSale,
		order,
	)
	if err != nil || !ready {
		return err
	}
	now := s.now().UTC()
	updated, err := s.repo.UpdateReturnVersioned(ctx, tx, *row, map[string]any{
		"status": StatusClosed, "closed_at": now, "active_delivery_order_id": nil,
		"settlement_status": "succeeded", "settled_at": now,
	})
	if err != nil {
		return err
	}
	if !updated {
		return problem.Conflict("VERSION_CONFLICT", "delivery return version changed")
	}
	if err := deliveryverification.Invalidate(ctx, tx, s.ids, row.DeliveryOrderID, "delivery_return_closed"); err != nil {
		return err
	}
	from := row.Status
	row.Status, row.ClosedAt, row.ActiveDeliveryOrderID, row.Version = StatusClosed, &now, nil, row.Version+1
	row.SettlementStatus, row.SettledAt = stringPointer("succeeded"), &now
	return s.writeFacts(ctx, tx, *row, "system", nil, "close", from, StatusClosed, "")
}

func (s *Service) TryCloseByAfterSaleWithTx(ctx context.Context, tx *gorm.DB, afterSaleID uint64) error {
	row, err := s.repo.ReturnByAfterSale(ctx, tx, afterSaleID, true)
	if IsNotFound(err) {
		return nil
	}
	if err != nil || (row.Status != StatusReceived && row.Status != StatusException) {
		return err
	}
	if row.SettlementType == nil || *row.SettlementType != SettlementRetailCashRefund {
		return problem.Internal("refund closure reached a non-cash delivery return")
	}
	refundStatus, err := s.repo.RefundStatus(ctx, tx, afterSaleID)
	if err != nil || refundStatus != "succeeded" {
		return err
	}
	complete, err := s.repo.ClosureComplete(ctx, tx, afterSaleID, row.ID)
	if err != nil || !complete {
		return err
	}
	now := s.now().UTC()
	updated, err := s.repo.UpdateReturnVersioned(ctx, tx, row, map[string]any{
		"status": StatusClosed, "closed_at": now, "active_delivery_order_id": nil,
		"settlement_status": "succeeded", "settled_at": now,
	})
	if err != nil || !updated {
		return err
	}
	if err := deliveryverification.Invalidate(ctx, tx, s.ids, row.DeliveryOrderID, "delivery_return_closed"); err != nil {
		return err
	}
	from := row.Status
	row.Status, row.ClosedAt, row.ActiveDeliveryOrderID, row.Version = StatusClosed, &now, nil, row.Version+1
	row.SettlementStatus, row.SettledAt = stringPointer("succeeded"), &now
	return s.writeFacts(ctx, tx, row, "system", nil, "close", from, StatusClosed, "")
}

// ReconcileRefundWithTx 在本地退款状态写入后，
// 由服务商回调和轮询工作进程共同调用。
func (s *Service) ReconcileRefundWithTx(ctx context.Context, tx *gorm.DB, afterSaleID uint64) error {
	status, err := s.repo.RefundStatus(ctx, tx, afterSaleID)
	if err != nil {
		return err
	}
	if status == "succeeded" {
		return s.TryCloseByAfterSaleWithTx(ctx, tx, afterSaleID)
	}
	if status != "failed" && status != "exception" {
		return nil
	}
	row, err := s.repo.ReturnByAfterSale(ctx, tx, afterSaleID, true)
	if IsNotFound(err) || (err == nil && row.Status == StatusException) {
		return nil
	}
	if err != nil || isTerminalStatus(row.Status) || row.Status == StatusDisputed {
		return err
	}
	from := row.Status
	updated, err := s.repo.UpdateReturnVersioned(ctx, tx, row, map[string]any{"status": StatusException, "settlement_status": "exception"})
	if err != nil || !updated {
		return err
	}
	row.Status, row.Version = StatusException, row.Version+1
	row.SettlementStatus = stringPointer("exception")
	return s.writeFacts(ctx, tx, row, "system", nil, "refund_exception", from, StatusException, "")
}

// CreateApproveFromIncidentWithTx 实现原子的异常 return_required 入口。
// 幂等性和异常事实由调用方负责。
func (s *Service) CreateApproveFromIncidentWithTx(ctx context.Context, tx *gorm.DB, incidentID, deliveryID, actorID uint64, note string) (uint64, error) {
	if !s.cfg.DeliveryReturn.Enabled || !s.cfg.DeliveryReturn.ApprovalEnabled || s.afterSales == nil {
		return 0, problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DISABLED", "Service Unavailable", "delivery return approval is disabled")
	}
	delivery, err := s.repo.LockDelivery(ctx, tx, deliveryID)
	if err != nil || delivery.Status != "delivering" || delivery.PickedUpAt == nil {
		return 0, problem.Conflict("INVALID_RETURN_STATE", "incident delivery is not returnable")
	}
	order, err := s.repo.LockOrder(ctx, tx, delivery.OrderID)
	if err != nil {
		return 0, problem.Conflict("INVALID_RETURN_STATE", "delivery return order is unavailable")
	}
	handler, err := s.settlementHandler(order)
	if err != nil {
		return 0, err
	}
	binding, err := handler.InitialBinding(ctx, tx, order)
	if err != nil {
		return 0, err
	}
	if err := validateReturnSettlementBinding(handler, binding); err != nil {
		return 0, err
	}
	incident, err := s.repo.Incident(ctx, tx, incidentID)
	if err != nil || incident.DeliveryOrderID != delivery.ID {
		return 0, problem.Conflict("INVALID_RETURN_STATE", "incident does not belong to delivery")
	}
	row, err := s.repo.ActiveByDelivery(ctx, tx, delivery.ID, true)
	if IsNotFound(err) {
		now := s.now().UTC()
		active := delivery.ID
		reason := reasonForIncident(incident.Type)
		row = Return{
			ID: s.ids.Next(), DeliveryOrderID: delivery.ID, ActiveDeliveryOrderID: &active,
			OrderID: delivery.OrderID, ShopID: delivery.ShopID, RiderID: valueOrZero(delivery.RiderID),
			IncidentID: &incidentID, ReasonCode: reason, Status: StatusRequested,
			SettlementType: optionalString(binding.SettlementType), SettlementBizID: binding.SettlementBizID,
			SettlementStatus: stringPointer("not_started"),
			InitiatorType:    "admin", InitiatorID: actorID, RequestNote: optionalString(note),
			RequestedAt: now, Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		row.ReturnNo = "DR" + idString(row.ID)
		if row.RiderID == 0 {
			return 0, problem.Conflict("INVALID_RETURN_STATE", "incident delivery has no rider")
		}
		if err := s.repo.CreateReturn(ctx, tx, &row); err != nil {
			return 0, err
		}
		if err := s.writeFacts(ctx, tx, row, "admin", &actorID, "request", "", StatusRequested, ""); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	} else if row.IncidentID != nil && *row.IncidentID != incidentID {
		return 0, problem.Conflict("RETURN_ALREADY_ACTIVE", "delivery has a return linked to another incident")
	}
	if err := validateReturnSettlementRoute(handler, row); err != nil {
		return 0, err
	}
	if row.Status == StatusReturning || row.Status == StatusArrived || row.Status == StatusReceived || row.Status == StatusClosed {
		return row.ID, nil
	}
	if row.Status != StatusRequested {
		return 0, problem.Conflict("INVALID_RETURN_STATE", "active delivery return requires manual review")
	}
	if err := s.approveReturnLocked(ctx, tx, &row, delivery, order, handler, actorID, note); err != nil {
		return 0, err
	}
	return row.ID, nil
}

func (s *Service) ValidateIncidentResolutionWithTx(ctx context.Context, tx *gorm.DB, incidentID uint64, resolutionCode string) error {
	if resolutionCode != "returned_to_store" {
		return nil
	}
	row, err := s.repo.ReturnByIncident(ctx, tx, incidentID, false)
	if err != nil || (row.Status != StatusReceived && row.Status != StatusClosed) {
		return problem.Conflict("INVALID_RETURN_STATE", "returned_to_store requires a real store receipt")
	}
	return nil
}

func (s *Service) ReturnIDForIncident(ctx context.Context, tx *gorm.DB, incidentID uint64) uint64 {
	row, err := s.repo.ReturnByIncident(ctx, tx, incidentID, false)
	if err != nil {
		return 0
	}
	return row.ID
}

func (s *Service) AdminDetail(ctx context.Context, claims *auth.Claims, idRaw string) (DTO, error) {
	if _, err := requireAdmin(claims, "delivery_return:view_all"); err != nil {
		return DTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return DTO{}, returnNotFound()
	}
	aggregate, err := s.repo.AdminAggregate(ctx, id)
	if IsNotFound(err) {
		return DTO{}, returnNotFound()
	}
	if err != nil {
		return DTO{}, err
	}
	return s.dto(aggregate, "admin"), nil
}

func (s *Service) StoreDetail(ctx context.Context, claims *auth.Claims, idRaw string) (DTO, error) {
	_, shops, err := requireStore(claims, "delivery_return:view_shop")
	if err != nil {
		return DTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return DTO{}, returnNotFound()
	}
	aggregate, err := s.repo.StoreAggregate(ctx, id, shops)
	if IsNotFound(err) {
		return DTO{}, returnNotFound()
	}
	if err != nil {
		return DTO{}, err
	}
	return s.dto(aggregate, "store"), nil
}

func (s *Service) AdminList(ctx context.Context, claims *auth.Claims, query ListQuery) ([]DTO, bool, error) {
	if _, err := requireAdmin(claims, "delivery_return:list_all"); err != nil {
		return nil, false, err
	}
	rows, err := s.repo.ListAdmin(ctx, query)
	return s.listDTO(ctx, rows, query.Limit, "admin", err)
}

func (s *Service) StoreList(ctx context.Context, claims *auth.Claims, query ListQuery) ([]DTO, bool, error) {
	_, shops, err := requireStore(claims, "delivery_return:list_shop")
	if err != nil {
		return nil, false, err
	}
	rows, err := s.repo.ListStore(ctx, shops, query)
	return s.listDTO(ctx, rows, query.Limit, "store", err)
}

func (s *Service) listDTO(ctx context.Context, rows []Return, limit int, role string, err error) ([]DTO, bool, error) {
	if err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]DTO, 0, len(rows))
	for _, row := range rows {
		aggregate, loadErr := s.repo.AdminAggregate(ctx, row.ID)
		if loadErr != nil {
			return nil, false, loadErr
		}
		out = append(out, s.dto(aggregate, role))
	}
	return out, hasMore, nil
}

func (s *Service) persistRejectedHandoff(ctx context.Context, tx *gorm.DB, row *Return, actorID uint64, route, key string, result persistedActionResult, reason string) error {
	attempts := row.HandoffFailedAttempts + 1
	updated, err := s.repo.UpdateReturnVersioned(ctx, tx, *row, map[string]any{"handoff_failed_attempts": attempts})
	if err != nil || !updated {
		return err
	}
	row.HandoffFailedAttempts, row.Version = attempts, row.Version+1
	now := s.now().UTC()
	if err := s.repo.CreateHistory(ctx, tx, History{
		ID: s.ids.Next(), DeliveryReturnID: row.ID, FromStatus: optionalString(row.Status), ToStatus: optionalString(row.Status),
		Action: "handoff_rejected", ActorType: "merchant", ActorID: &actorID,
		RequestID: requestctx.RequestIDPtr(ctx), IdempotencyKey: optionalString(key),
		MetadataJSON: jsonData(map[string]any{"reason": reason, "attempts": attempts}), CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := s.repo.CreateAudit(ctx, tx, AuditLog{
		ID: s.ids.Next(), ActorType: "merchant", ActorID: actorID, Action: "delivery_return.handoff_rejected",
		ResourceType: "delivery_return", ResourceID: row.ID,
		AfterData: jsonData(map[string]any{"reason": reason, "attempts": attempts}), Result: "denied",
		RequestID: requestctx.RequestIDPtr(ctx), IPHash: requestctx.IPHashPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx),
	}); err != nil {
		return err
	}
	result.DTO = s.dto(Aggregate{Return: *row, RefundStatus: "creating"}, "store")
	return s.idem.Succeed(ctx, tx, "merchant", actorID, route, key, result)
}

func (s *Service) persistQuantityDispute(ctx context.Context, tx *gorm.DB, row *Return, actorID uint64, route, key string, result *persistedActionResult) error {
	from := row.Status
	updated, err := s.repo.UpdateReturnVersioned(ctx, tx, *row, map[string]any{"status": StatusDisputed})
	if err != nil || !updated {
		return err
	}
	row.Status, row.Version = StatusDisputed, row.Version+1
	if err := s.writeFacts(ctx, tx, *row, "merchant", &actorID, "manual_review", from, StatusDisputed, key); err != nil {
		return err
	}
	aggregate, err := s.repo.AggregateTx(ctx, tx, row.ID)
	if err != nil {
		return err
	}
	*result = persistedActionResult{DTO: s.dto(aggregate, "store"), ErrorCode: "MANUAL_REVIEW_REQUIRED", ErrorDetail: "received quantities do not match the approved return"}
	return s.idem.Succeed(ctx, tx, "merchant", actorID, route, key, *result)
}

func validateReceiveRequest(req *ReceiveReq) error {
	seen := make(map[uint64]bool, len(req.Items))
	for index := range req.Items {
		id, err := parseID(req.Items[index].AfterSaleItemID)
		if err != nil || seen[id] {
			return problem.InvalidArgument("VALIDATION_FAILED", "after_sale_item_id must be unique and positive")
		}
		seen[id] = true
		note, err := cleanNote(req.Items[index].Note)
		if err != nil {
			return err
		}
		req.Items[index].Note = note
	}
	return nil
}

func (s *Service) cachedAction(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, route, key string, out *persistedActionResult) error {
	ok, err := s.idem.CachedResponse(ctx, tx, actorType, actorID, route, key, out)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
	}
	return nil
}

func (s *Service) checkRate(ctx context.Context, action string, actorID uint64, window time.Duration, limit int64) error {
	result := s.limiter.Allow(ctx, "rate:delivery_return:"+action+":"+idString(actorID), window, limit)
	if result.Degraded {
		return problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DEPENDENCY_UNAVAILABLE", "Service Unavailable", "delivery return rate limiter is unavailable")
	}
	if !result.Allowed {
		err := problem.TooManyRequests("RATE_LIMITED", "delivery return action rate exceeded")
		err.Data = map[string]any{"retry_after_seconds": int(result.RetryAfter.Seconds())}
		return err
	}
	return nil
}

func requireAdmin(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" || !hasPermission(claims.Permissions, permission) {
		return 0, problem.Forbidden("RETURN_FORBIDDEN", "admin permission denied")
	}
	id, err := parseID(claims.AdminUserID)
	if err != nil {
		return 0, problem.Forbidden("RETURN_FORBIDDEN", "invalid admin identity")
	}
	return id, nil
}

func requireStore(claims *auth.Claims, permission string) (uint64, []uint64, error) {
	if claims == nil || claims.AccountType != "merchant" || !hasPermission(claims.Permissions, permission) {
		return 0, nil, problem.Forbidden("RETURN_FORBIDDEN", "merchant permission denied")
	}
	actorID, err := parseID(claims.MerchantUserID)
	if err != nil {
		return 0, nil, problem.Forbidden("RETURN_FORBIDDEN", "invalid merchant identity")
	}
	shops := make([]uint64, 0, len(claims.AuthorizedShopIDs))
	for _, raw := range claims.AuthorizedShopIDs {
		id, err := parseID(raw)
		if err != nil {
			return 0, nil, problem.Forbidden("RETURN_FORBIDDEN", "invalid shop scope")
		}
		shops = append(shops, id)
	}
	if len(shops) == 0 {
		return 0, nil, problem.Forbidden("RETURN_FORBIDDEN", "no authorized shops")
	}
	return actorID, shops, nil
}

func returnPolicy(raw []byte) (string, string, bool) {
	var snapshot struct {
		ReturnPolicy *struct {
			Eligible      bool   `json:"eligible"`
			PolicyCode    string `json:"policy_code"`
			PolicyVersion string `json:"policy_version"`
		} `json:"return_policy"`
	}
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.ReturnPolicy == nil {
		return "not_configured", "1", false
	}
	code := strings.TrimSpace(snapshot.ReturnPolicy.PolicyCode)
	version := strings.TrimSpace(snapshot.ReturnPolicy.PolicyVersion)
	if code == "" {
		code = "not_configured"
	}
	if version == "" {
		version = "1"
	}
	return code, version, snapshot.ReturnPolicy.Eligible
}

func newHandoffCode() (string, error) {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for index := range raw {
		raw[index] = alphabet[int(raw[index])%len(alphabet)]
	}
	return string(raw), nil
}

func reasonForIncident(kind string) string {
	switch kind {
	case "customer_unreachable":
		return ReasonCustomerUnreachable
	case "customer_refused":
		return ReasonCustomerRefused
	case "alcohol_damaged":
		return ReasonDamagedInTransit
	default:
		return ReasonOther
	}
}

func aggregateDisposition(items []ReceiptItem) string {
	if len(items) == 0 {
		return "mixed"
	}
	value := items[0].Disposition
	for _, item := range items[1:] {
		if item.Disposition != value {
			return "mixed"
		}
	}
	return value
}

func uniqueIDs(values []uint64) []uint64 {
	seen := make(map[uint64]bool, len(values))
	out := make([]uint64, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func containsID(values []uint64, expected uint64) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func valueOrZero(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

func stringPointer(value string) *string { return &value }
