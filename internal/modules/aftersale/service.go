package aftersale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/evidencetoken"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg  config.Config
	repo *Repository
	ids  *snowflake.Generator
	idem *idempotency.Store
	now  func() time.Time
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{cfg: cfg, repo: NewRepository(db), ids: ids, idem: idempotency.NewStore(db), now: time.Now}
}

// Create 创建DTO。
func (s *Service) Create(ctx context.Context, claims *auth.Claims, method, path, key string, req CreateReq) (DTO, error) {
	if !s.cfg.AfterSale.Enabled {
		return DTO{}, problem.New(503, "AFTER_SALE_DISABLED", "Service Unavailable", "after-sale applications are disabled")
	}
	customerID, err := customerActor(claims)
	if err != nil {
		return DTO{}, err
	}
	orderID, err := parseID(req.OrderID)
	if err != nil {
		return DTO{}, problem.NotFound("ORDER_NOT_FOUND", "order not found")
	}
	if req.Type == "damaged" && len(req.EvidenceTokens) == 0 {
		return DTO{}, problem.New(422, "AFTER_SALE_EVIDENCE_REQUIRED", "Unprocessable Entity", "damaged items require evidence")
	}
	if !resolutionAllowed(req.Type, req.RequestedResolution) {
		return DTO{}, problem.InvalidArgument("AFTER_SALE_RESOLUTION_INVALID", "requested resolution is not allowed for after-sale type")
	}
	if req.IncludeDeliveryFee && req.Type != "out_of_stock" && req.Type != "other" {
		return DTO{}, problem.InvalidArgument("AFTER_SALE_DELIVERY_FEE_INVALID", "delivery fee refund is not allowed for after-sale type")
	}
	var out DTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cached(ctx, tx, claims.AccountType, customerID, path, key, &out)
		}
		now := s.now().UTC()
		orderCount, customerCount, err := s.repo.CreateRateCounts(ctx, tx, customerID, orderID, now.Add(-time.Hour), now.Add(-24*time.Hour))
		if err != nil {
			return err
		}
		if orderCount >= 5 || customerCount >= 20 {
			return problem.TooManyRequests("AFTER_SALE_RATE_LIMITED", "too many after-sale applications")
		}
		orderRow, err := s.repo.LockOrder(ctx, tx, orderID)
		if errors.Is(err, gorm.ErrRecordNotFound) || orderRow.CustomerID != customerID {
			return problem.NotFound("ORDER_NOT_FOUND", "order not found")
		}
		if err != nil {
			return err
		}
		if err := s.eligible(orderRow, req.Type); err != nil {
			return err
		}
		ids := make([]uint64, 0, len(req.Items))
		seen := map[uint64]bool{}
		for _, v := range req.Items {
			id, e := parseID(v.OrderItemID)
			if e != nil || seen[id] {
				return problem.InvalidArgument("VALIDATION_FAILED", "invalid or duplicate order_item_id")
			}
			seen[id] = true
			ids = append(ids, id)
		}
		orderItems, err := s.repo.OrderItems(ctx, tx, orderID, ids)
		if err != nil {
			return err
		}
		if len(orderItems) != len(ids) {
			return problem.New(422, "AFTER_SALE_AMOUNT_EXCEEDED", "Unprocessable Entity", "order item is invalid")
		}
		initialStatus := "submitted"
		if req.Type == "unopened_return" {
			for _, item := range orderItems {
				known, eligible := returnPolicy(item.ProductSnapshot)
				if !known {
					initialStatus = "platform_reviewing"
					continue
				}
				if !eligible {
					return problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "item is not eligible for unopened return")
				}
			}
		}
		active, err := s.repo.ActiveRequested(ctx, tx, ids)
		if err != nil {
			return err
		}
		byID := map[uint64]OrderItemRow{}
		for _, v := range orderItems {
			byID[v.ID] = v
		}
		afterID := s.ids.Next()
		items := make([]Item, 0, len(req.Items))
		var amount int64
		for _, requested := range req.Items {
			id, _ := parseID(requested.OrderItemID)
			base := byID[id]
			used := active[id]
			maxQty := base.Quantity - used.Quantity
			maxAmount := base.TotalAmount - used.Amount
			if requested.Quantity > maxQty {
				return problem.Conflict("AFTER_SALE_DUPLICATE_ACTIVE", "order item quantity is already in after-sale processing")
			}
			proportional := base.TotalAmount * int64(requested.Quantity) / int64(base.Quantity)
			if requested.RequestedAmount > maxAmount || requested.RequestedAmount > proportional {
				return problem.New(422, "AFTER_SALE_AMOUNT_EXCEEDED", "Unprocessable Entity", "requested amount exceeds refundable item amount")
			}
			amount += requested.RequestedAmount
			items = append(items, Item{ID: s.ids.Next(), AfterSaleID: afterID, OrderID: orderID, OrderItemID: id, ShopProductID: base.ShopProductID, ProductID: base.ProductID, RequestedQuantity: requested.Quantity, RequestedAmount: requested.RequestedAmount, ReturnDisposition: "none"})
		}
		if req.IncludeDeliveryFee {
			claimed, e := s.repo.DeliveryFeeClaimed(ctx, tx, orderID)
			if e != nil {
				return e
			}
			if claimed {
				return problem.New(422, "AFTER_SALE_AMOUNT_EXCEEDED", "Unprocessable Entity", "delivery fee was already claimed")
			}
			amount += orderRow.DeliveryFeeAmount
		}
		if amount <= 0 && req.RequestedResolution != "replacement" && req.RequestedResolution != "compensation" {
			return problem.New(422, "AFTER_SALE_AMOUNT_EXCEEDED", "Unprocessable Entity", "requested amount must be positive")
		}
		if amount >= s.cfg.AfterSale.PlatformReviewThreshold || req.Type == "late_delivery" || req.RequestedResolution == "compensation" || orderCount > 0 || customerCount > 0 {
			initialStatus = "platform_reviewing"
		}
		now = s.now().UTC()
		row := AfterSale{ID: afterID, AfterSaleNo: "AS" + fmt.Sprint(afterID), OrderID: orderID, CustomerID: customerID, MerchantID: orderRow.MerchantID, ShopID: orderRow.ShopID, InitiatorType: "customer", SourceType: "customer_request", Type: req.Type, RequestedResolution: req.RequestedResolution, Status: initialStatus, RequestedAmount: amount, IncludeDeliveryFee: req.IncludeDeliveryFee, Description: strings.TrimSpace(req.Description), SubmittedAt: now, Version: 1}
		evidence, err := s.evidence(afterID, customerID, req.EvidenceTokens)
		if err != nil {
			return err
		}
		history := s.history(afterID, "customer", customerID, "submit", nil, strPtr(initialStatus), "", "")
		audit := s.audit(ctx, "customer", customerID, "after_sale.create", afterID, nil, row)
		outbox := s.outbox(ctx, "after_sale.submitted", afterID, map[string]any{"after_sale_id": idString(afterID), "order_id": idString(orderID)})
		if err := s.repo.Create(ctx, tx, &row, items, evidence, history, audit, outbox); err != nil {
			return err
		}
		if err := s.repo.UpdateOrderSummary(ctx, tx, orderID, "pending"); err != nil {
			return err
		}
		out = s.dto(row, items, evidence, nil)
		return s.idem.Succeed(ctx, tx, claims.AccountType, customerID, path, key, out)
	})
	return out, err
}

// returnPolicy 返回return 策略。
func returnPolicy(snapshot datatypes.JSON) (known bool, eligible bool) {
	var value struct {
		ReturnPolicy *struct {
			Eligible bool `json:"eligible"`
		} `json:"return_policy"`
	}
	if json.Unmarshal(snapshot, &value) != nil || value.ReturnPolicy == nil {
		return false, false
	}
	return true, value.ReturnPolicy.Eligible
}

// eligible 返回eligible。
func (s *Service) eligible(row OrderRow, kind string) error {
	if row.PayStatus != "paid" && row.Status != "paid" && row.Status != "accepted" && row.Status != "preparing" && row.Status != "ready_for_pickup" && row.Status != "delivering" && row.Status != "completed" {
		return problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "order is not eligible for after-sale")
	}
	if row.Status == "completed" && row.CompletedAt != nil {
		window := s.cfg.AfterSale.StandardWindow
		if kind == "unopened_return" {
			window = s.cfg.AfterSale.UnopenedReturnWindow
		}
		if s.now().After(row.CompletedAt.Add(window)) {
			return problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "after-sale application window expired")
		}
	}
	if kind == "unopened_return" && row.Status != "completed" {
		return problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "unopened return requires a completed order")
	}
	return nil
}

// ListCustomer 查询用户列表。
func (s *Service) ListCustomer(ctx context.Context, claims *auth.Claims, q ListQuery) ([]DTO, string, error) {
	id, e := customerActor(claims)
	if e != nil {
		return nil, "", e
	}
	rows, e := s.repo.ListCustomer(ctx, id, q)
	return s.page(rows, q, e)
}

// ListStore 查询门店列表。
func (s *Service) ListStore(ctx context.Context, claims *auth.Claims, q ListQuery) ([]DTO, string, error) {
	a, e := storeActor(claims, "after_sale:list_shop")
	if e != nil {
		return nil, "", e
	}
	rows, e := s.repo.ListStore(ctx, a.merchantID, a.shopIDs, q)
	return s.page(rows, q, e)
}

// ListAdmin 查询管理端列表。
func (s *Service) ListAdmin(ctx context.Context, claims *auth.Claims, q ListQuery) ([]DTO, string, error) {
	if _, e := adminActor(claims, "after_sale:list_all"); e != nil {
		return nil, "", e
	}
	rows, e := s.repo.ListAdmin(ctx, q)
	return s.page(rows, q, e)
}

// page 返回分页。
func (s *Service) page(rows []AfterSale, q ListQuery, err error) ([]DTO, string, error) {
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > q.PageSize {
		rows = rows[:q.PageSize]
		next = pagination.NextPageToken(q.Query)
	}
	return s.rowsDTO(rows), next, nil
}

// DetailCustomer 返回Detail 用户。
func (s *Service) DetailCustomer(ctx context.Context, claims *auth.Claims, idRaw string) (DTO, error) {
	actor, e := customerActor(claims)
	if e != nil {
		return DTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return DTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	row, e := s.repo.Owned(ctx, s.repo.DB(), actor, id, false)
	return s.detail(ctx, row, e)
}

// DetailStore 返回Detail 门店。
func (s *Service) DetailStore(ctx context.Context, claims *auth.Claims, idRaw string) (DTO, error) {
	a, e := storeActor(claims, "after_sale:view_shop")
	if e != nil {
		return DTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return DTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	row, e := s.repo.Scoped(ctx, s.repo.DB(), id, a.merchantID, a.shopIDs, false)
	return s.detail(ctx, row, e)
}

// DetailAdmin 返回Detail 管理端。
func (s *Service) DetailAdmin(ctx context.Context, claims *auth.Claims, idRaw string) (DTO, error) {
	if _, e := adminActor(claims, "after_sale:view_all"); e != nil {
		return DTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return DTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	row, e := s.repo.Any(ctx, s.repo.DB(), id, false)
	return s.detail(ctx, row, e)
}

// detail 返回detail。
func (s *Service) detail(ctx context.Context, row AfterSale, err error) (DTO, error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	if err != nil {
		return DTO{}, err
	}
	items, e := s.repo.Items(ctx, s.repo.DB(), row.ID)
	if e != nil {
		return DTO{}, e
	}
	ev, e := s.repo.Evidence(ctx, s.repo.DB(), row.ID)
	if e != nil {
		return DTO{}, e
	}
	h, e := s.repo.History(ctx, s.repo.DB(), row.ID)
	if e != nil {
		return DTO{}, e
	}
	return s.dto(row, items, ev, h), nil
}

// AddEvidence 添加Evidence。
func (s *Service) AddEvidence(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req EvidenceReq) (DTO, error) {
	actor, e := customerActor(claims)
	if e != nil {
		return DTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return DTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	var out DTO
	e = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actor, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return s.cached(ctx, tx, claims.AccountType, actor, path, key, &out)
		}
		count, e := s.repo.HistoryActionCount(ctx, tx, "customer", actor, "add_evidence", s.now().Add(-time.Hour))
		if e != nil {
			return e
		}
		if count >= 30 {
			return problem.TooManyRequests("AFTER_SALE_RATE_LIMITED", "evidence append rate limit exceeded")
		}
		row, e := s.repo.Owned(ctx, tx, actor, id, true)
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
		}
		if e != nil {
			return e
		}
		if row.Version != req.Version {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		if row.Status != "submitted" && row.Status != "evidence_required" {
			return problem.Conflict("AFTER_SALE_STATUS_CONFLICT", "evidence cannot be added in current status")
		}
		ev, e := s.evidence(id, actor, req.EvidenceTokens)
		if e != nil {
			return e
		}
		if e = s.repo.AddEvidence(ctx, tx, ev); e != nil {
			return e
		}
		target := "submitted"
		ok, e := s.repo.UpdateCAS(ctx, tx, id, req.Version, map[string]any{"status": target})
		if e != nil {
			return e
		}
		if !ok {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		if e = s.repo.CreateHistory(ctx, tx, s.history(id, "customer", actor, "add_evidence", strPtr(row.Status), strPtr(target), "", "")); e != nil {
			return e
		}
		if e = s.repo.CreateAudit(ctx, tx, s.audit(ctx, "customer", actor, "after_sale.add_evidence", id, row, map[string]any{"status": target, "evidence_count": len(ev)})); e != nil {
			return e
		}
		if e = s.repo.CreateOutbox(ctx, tx, s.outbox(ctx, "after_sale.evidence_added", id, map[string]any{"after_sale_id": idString(id), "evidence_count": len(ev)})); e != nil {
			return e
		}
		row.Status = target
		row.Version++
		out = s.dto(row, nil, ev, nil)
		return s.idem.Succeed(ctx, tx, claims.AccountType, actor, path, key, out)
	})
	return out, e
}

// Withdraw 返回Withdraw。
func (s *Service) Withdraw(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req WithdrawReq) (DTO, error) {
	actor, e := customerActor(claims)
	if e != nil {
		return DTO{}, e
	}
	return s.customerTransition(ctx, claims, actor, method, path, key, idRaw, req.Version, "withdraw", []string{"submitted", "evidence_required", "shop_reviewing"}, "withdrawn", req.Reason, nil, nil)
}

// Appeal 返回Appeal。
func (s *Service) Appeal(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req AppealReq) (DTO, error) {
	actor, e := customerActor(claims)
	if e != nil {
		return DTO{}, e
	}
	now := s.now().UTC()
	return s.customerTransition(ctx, claims, actor, method, path, key, idRaw, req.Version, "appeal", []string{"rejected"}, "platform_reviewing", req.Remark, map[string]any{"appealed_at": &now}, req.EvidenceTokens)
}

// customerTransition 返回用户状态流转。
func (s *Service) customerTransition(ctx context.Context, claims *auth.Claims, actor uint64, method, path, key, idRaw string, version uint32, action string, allowed []string, target, remark string, extra map[string]any, tokens []string) (DTO, error) {
	id, e := parseID(idRaw)
	if e != nil {
		return DTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	var out DTO
	e = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actor, method, path, key, idempotency.RequestHash(map[string]any{"id": id, "version": version, "action": action, "remark": remark}))
		if e != nil {
			return e
		}
		if !started {
			return s.cached(ctx, tx, claims.AccountType, actor, path, key, &out)
		}
		row, e := s.repo.Owned(ctx, tx, actor, id, true)
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
		}
		if e != nil {
			return e
		}
		if row.Version != version {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		valid := false
		for _, v := range allowed {
			if row.Status == v {
				valid = true
			}
		}
		if !valid || (action == "appeal" && row.AppealedAt != nil) {
			return problem.Conflict("AFTER_SALE_STATUS_CONFLICT", "action is not allowed")
		}
		var evidence []Evidence
		if len(tokens) > 0 {
			evidence, e = s.evidence(id, actor, tokens)
			if e != nil {
				return e
			}
			if e = s.repo.AddEvidence(ctx, tx, evidence); e != nil {
				return e
			}
		}
		values := map[string]any{"status": target}
		for k, v := range extra {
			values[k] = v
		}
		ok, e := s.repo.UpdateCAS(ctx, tx, id, version, values)
		if e != nil {
			return e
		}
		if !ok {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		if e = s.repo.CreateHistory(ctx, tx, s.history(id, "customer", actor, action, strPtr(row.Status), strPtr(target), "", remark)); e != nil {
			return e
		}
		if e = s.repo.CreateAudit(ctx, tx, s.audit(ctx, "customer", actor, "after_sale."+action, id, row, values)); e != nil {
			return e
		}
		if e = s.repo.CreateOutbox(ctx, tx, s.outbox(ctx, "after_sale.updated", id, map[string]any{"after_sale_id": idString(id), "status": target})); e != nil {
			return e
		}
		row.Status = target
		row.Version++
		out = s.dto(row, nil, evidence, nil)
		return s.idem.Succeed(ctx, tx, claims.AccountType, actor, path, key, out)
	})
	return out, e
}

type storeIdentity struct {
	userID, merchantID uint64
	shopIDs            []uint64
}

// ReviewStore 审核门店。
func (s *Service) ReviewStore(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ReviewReq) (DTO, error) {
	a, e := storeActor(claims, "after_sale:review_shop")
	if e != nil {
		return DTO{}, e
	}
	return s.review(ctx, claims, "merchant", a.userID, method, path, key, idRaw, req, func(tx *gorm.DB, id uint64) (AfterSale, error) {
		return s.repo.Scoped(ctx, tx, id, a.merchantID, a.shopIDs, true)
	})
}

// ReviewAdmin 审核管理端。
func (s *Service) ReviewAdmin(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ReviewReq) (DTO, error) {
	actor, e := adminActor(claims, "after_sale:review_platform")
	if e != nil {
		return DTO{}, e
	}
	if req.Decision == "approve" && req.Resolution == "compensation" {
		if _, e := adminActor(claims, "compensation:approve"); e != nil {
			return DTO{}, e
		}
	}
	return s.review(ctx, claims, "admin", actor, method, path, key, idRaw, req, func(tx *gorm.DB, id uint64) (AfterSale, error) { return s.repo.Any(ctx, tx, id, true) })
}

// review 审核DTO。
func (s *Service) review(ctx context.Context, claims *auth.Claims, actorType string, actorID uint64, method, path, key, idRaw string, req ReviewReq, load func(*gorm.DB, uint64) (AfterSale, error)) (DTO, error) {
	id, e := parseID(idRaw)
	if e != nil {
		return DTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	var out DTO
	e = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actorID, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return s.cached(ctx, tx, claims.AccountType, actorID, path, key, &out)
		}
		count, e := s.repo.HistoryActionCount(ctx, tx, actorType, actorID, "review_%", s.now().Add(-time.Minute))
		if e != nil {
			return e
		}
		if count >= 60 {
			return problem.TooManyRequests("AFTER_SALE_RATE_LIMITED", "review rate limit exceeded")
		}
		row, e := load(tx, id)
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
		}
		if e != nil {
			return e
		}
		if row.Version != req.Version {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		if actorType == "merchant" && row.Status == "platform_reviewing" {
			return problem.Conflict("AFTER_SALE_PLATFORM_REVIEW_REQUIRED", "after-sale requires platform review")
		}
		if row.Status != "submitted" && row.Status != "shop_reviewing" && row.Status != "platform_reviewing" && row.Status != "appealed" {
			return problem.Conflict("AFTER_SALE_STATUS_CONFLICT", "review is not allowed")
		}
		items, e := s.repo.Items(ctx, tx, id)
		if e != nil {
			return e
		}
		target := ""
		values := map[string]any{"reason_code": optional(req.ReasonCode)}
		switch req.Decision {
		case "reject":
			if strings.TrimSpace(req.Remark) == "" {
				return problem.InvalidArgument("VALIDATION_FAILED", "reject remark is required")
			}
			target = "rejected"
			now := s.now().UTC()
			values["rejected_at"] = &now
		case "request_evidence":
			target = "evidence_required"
		case "escalate":
			if strings.TrimSpace(req.Remark) == "" {
				return problem.InvalidArgument("VALIDATION_FAILED", "escalation remark is required")
			}
			target = "platform_reviewing"
		case "approve":
			resolution := req.Resolution
			if resolution == "" {
				resolution = row.RequestedResolution
			}
			if actorType == "admin" && resolution == "compensation" && !hasPermission(claims, "compensation:approve") {
				return problem.Forbidden("PERM_FORBIDDEN", "compensation approval permission denied")
			}
			if !resolutionAllowed(row.Type, resolution) {
				return problem.InvalidArgument("AFTER_SALE_RESOLUTION_INVALID", "approved resolution is not allowed for after-sale type")
			}
			quarantined, e := s.repo.QuarantinedEvidenceCount(ctx, tx, row.ID)
			if e != nil {
				return e
			}
			if quarantined > 0 {
				return problem.Conflict("AFTER_SALE_EVIDENCE_QUARANTINED", "evidence security scan is not complete")
			}
			return s.approve(ctx, tx, row, items, req, actorType, actorID, claims, path, key, &out)
		default:
			return problem.InvalidArgument("VALIDATION_FAILED", "invalid review decision")
		}
		values["status"] = target
		ok, e := s.repo.UpdateCAS(ctx, tx, id, req.Version, values)
		if e != nil {
			return e
		}
		if !ok {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		if e = s.repo.CreateHistory(ctx, tx, s.history(id, actorType, actorID, "review_"+req.Decision, strPtr(row.Status), strPtr(target), req.ReasonCode, req.Remark)); e != nil {
			return e
		}
		if e = s.repo.CreateAudit(ctx, tx, s.audit(ctx, actorType, actorID, "after_sale.review", id, row, values)); e != nil {
			return e
		}
		if e = s.repo.CreateOutbox(ctx, tx, s.outbox(ctx, "after_sale.updated", id, map[string]any{"after_sale_id": idString(id), "status": target})); e != nil {
			return e
		}
		row.Status = target
		row.Version++
		out = s.dto(row, items, nil, nil)
		return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, out)
	})
	return out, e
}

// resolutionAllowed 判断resolution 允许状态。
func resolutionAllowed(kind, resolution string) bool {
	switch kind {
	case "unopened_return":
		return resolution == "return_and_refund"
	case "damaged", "missing_item":
		return resolution == "refund_only" || resolution == "replacement"
	case "out_of_stock":
		return resolution == "refund_only"
	case "late_delivery":
		return resolution == "compensation"
	case "other":
		return resolution == "refund_only" || resolution == "return_and_refund" || resolution == "replacement" || resolution == "compensation"
	default:
		return false
	}
}

// approve 审批通过售后。
func (s *Service) approve(ctx context.Context, tx *gorm.DB, row AfterSale, items []Item, req ReviewReq, actorType string, actorID uint64, claims *auth.Claims, path, key string, out *DTO) error {
	resolution := req.Resolution
	if resolution == "" {
		resolution = row.RequestedResolution
	}
	approved := map[uint64]ApprovedItemReq{}
	for _, v := range req.ApprovedItems {
		id, e := parseID(v.AfterSaleItemID)
		if e != nil {
			return problem.InvalidArgument("VALIDATION_FAILED", "invalid after_sale_item_id")
		}
		approved[id] = v
	}
	var total int64
	for i := range items {
		v, ok := approved[items[i].ID]
		if !ok {
			items[i].ApprovedQuantity = items[i].RequestedQuantity
			items[i].ApprovedAmount = items[i].RequestedAmount
		} else {
			if v.Quantity > items[i].RequestedQuantity || v.Amount > items[i].RequestedAmount {
				return problem.New(422, "AFTER_SALE_AMOUNT_EXCEEDED", "Unprocessable Entity", "approved amount exceeds request")
			}
			items[i].ApprovedQuantity = v.Quantity
			items[i].ApprovedAmount = v.Amount
		}
		total += items[i].ApprovedAmount
	}
	if req.RefundDeliveryFee {
		orderRow, e := s.repo.LockOrder(ctx, tx, row.OrderID)
		if e != nil {
			return e
		}
		if !row.IncludeDeliveryFee {
			return problem.New(422, "AFTER_SALE_AMOUNT_EXCEEDED", "Unprocessable Entity", "delivery fee was not requested")
		}
		total += orderRow.DeliveryFeeAmount
	}
	now := s.now().UTC()
	target := ""
	switch resolution {
	case "refund_only":
		target = "refund_processing"
		if e := s.createRefundTask(ctx, tx, row, items, total, now); e != nil {
			return e
		}
	case "return_and_refund":
		target = "waiting_return"
	case "replacement":
		target = "replacement_processing"
		orderRow, e := s.repo.LockOrder(ctx, tx, row.OrderID)
		if e != nil {
			return e
		}
		payload, _ := json.Marshal(items)
		replacementID := s.ids.Next()
		e = s.repo.CreateReplacement(ctx, tx, Replacement{ID: replacementID, ReplacementNo: "RP" + idString(replacementID), AfterSaleID: row.ID, OriginalOrderID: row.OrderID, ShopID: row.ShopID, Status: "created", AddressSnapshot: orderRow.AddressSnapshot, ItemsJSON: datatypes.JSON(payload), Version: 1})
		if e != nil {
			return e
		}
	case "compensation":
		target = "compensation_processing"
		compID := s.ids.Next()
		reason := req.Remark
		if err := s.repo.CreateCompensation(ctx, tx, Compensation{ID: compID, CompensationNo: "CP" + idString(compID), AfterSaleID: row.ID, CustomerID: row.CustomerID, Type: "late_delivery", Amount: total, AssetType: "manual_credit_pending", Status: "approved", Reason: &reason}); err != nil {
			return err
		}
	default:
		return problem.InvalidArgument("VALIDATION_FAILED", "invalid resolution")
	}
	if e := s.repo.ApproveItems(ctx, tx, items); e != nil {
		return e
	}
	values := map[string]any{"status": target, "approved_resolution": resolution, "approved_amount": total, "approved_at": &now, "reason_code": optional(req.ReasonCode)}
	if resolution == "compensation" {
		values["compensation_amount"] = total
	}
	ok, e := s.repo.UpdateCAS(ctx, tx, row.ID, req.Version, values)
	if e != nil {
		return e
	}
	if !ok {
		return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
	}
	if e = s.repo.UpdateOrderSummary(ctx, tx, row.OrderID, "processing"); e != nil {
		return e
	}
	if e = s.repo.CreateHistory(ctx, tx, s.history(row.ID, actorType, actorID, "review_approve", strPtr(row.Status), strPtr(target), req.ReasonCode, req.Remark)); e != nil {
		return e
	}
	if e = s.repo.CreateAudit(ctx, tx, s.audit(ctx, actorType, actorID, "after_sale.approve", row.ID, row, values)); e != nil {
		return e
	}
	if e = s.repo.CreateOutbox(ctx, tx, s.outbox(ctx, "after_sale.approved", row.ID, map[string]any{"after_sale_id": idString(row.ID), "resolution": resolution})); e != nil {
		return e
	}
	row.Status = target
	row.ApprovedResolution = &resolution
	row.ApprovedAmount = total
	row.Version++
	*out = s.dto(row, items, nil, nil)
	return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, *out)
}

// createRefundTask 创建退款任务。
func (s *Service) createRefundTask(ctx context.Context, tx *gorm.DB, row AfterSale, items []Item, total int64, now time.Time) error {
	if !s.cfg.AfterSale.RefundExecutionEnabled {
		return problem.New(503, "REFUND_EXECUTION_DISABLED", "Service Unavailable", "refund execution is disabled")
	}
	payment, err := s.repo.LockPayment(ctx, tx, row.OrderID)
	if err != nil {
		return problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "succeeded payment not found")
	}
	reserved, err := s.repo.ReservedRefund(ctx, tx, payment.ID)
	if err != nil {
		return err
	}
	if total <= 0 || payment.RefundedAmount+reserved+total > payment.Amount {
		return problem.Conflict("REFUND_AMOUNT_EXCEEDED", "refund exceeds payment amount")
	}
	refundID := s.ids.Next()
	refund := Refund{ID: refundID, RefundNo: "RF" + idString(refundID), AfterSaleID: row.ID, OrderID: row.OrderID, PaymentID: payment.ID, Provider: payment.Provider, Status: "creating", Amount: total, TotalAmount: payment.Amount, Currency: payment.Currency, RequestedAt: now, NextRetryAt: &now, Version: 1}
	refundItems := make([]RefundItem, 0, len(items))
	for _, item := range items {
		if item.ApprovedAmount > 0 {
			refundItems = append(refundItems, RefundItem{ID: s.ids.Next(), RefundID: refundID, AfterSaleItemID: item.ID, Amount: item.ApprovedAmount, Quantity: item.ApprovedQuantity})
		}
	}
	return s.repo.CreateRefund(ctx, tx, refund, refundItems)
}

// ReceiveReturn 接收并处理Return。
func (s *Service) ReceiveReturn(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ReturnReceiptReq) (ReturnReceiptDTO, error) {
	actor, err := storeActor(claims, "after_sale:receive_return")
	if err != nil {
		return ReturnReceiptDTO{}, err
	}
	afterSaleID, err := parseID(idRaw)
	if err != nil {
		return ReturnReceiptDTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	var out ReturnReceiptDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actor.userID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cached(ctx, tx, claims.AccountType, actor.userID, path, key, &out)
		}
		row, err := s.repo.Scoped(ctx, tx, afterSaleID, actor.merchantID, actor.shopIDs, true)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
		}
		if err != nil {
			return err
		}
		if row.Version != req.Version {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		if row.Status != "waiting_return" || row.ApprovedResolution == nil || *row.ApprovedResolution != "return_and_refund" {
			return problem.Conflict("AFTER_SALE_STATUS_CONFLICT", "return receipt is not allowed")
		}
		items, err := s.repo.Items(ctx, tx, row.ID)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		receiptID := s.ids.Next()
		receipt := ReturnReceipt{ID: receiptID, ReceiptNo: "RR" + idString(receiptID), AfterSaleID: row.ID, ShopID: row.ShopID, Disposition: req.Disposition, SealedPackageIntact: req.SealedPackageIntact, GoodsIntact: req.GoodsIntact, Remark: optional(req.Remark), ReceivedBy: actor.userID, ReceivedAt: now}
		if err := s.repo.CreateReturnReceipt(ctx, tx, receipt); err != nil {
			return err
		}
		target := "refund_processing"
		if row.Type == "unopened_return" && (!req.SealedPackageIntact || !req.GoodsIntact) {
			target = "platform_reviewing"
		}
		if target == "refund_processing" {
			if req.Disposition == "restock" {
				quantities := map[uint64]int{}
				products := map[uint64]uint64{}
				for _, item := range items {
					quantities[item.ShopProductID] += item.ApprovedQuantity
					products[item.ShopProductID] = item.ProductID
				}
				ids := make([]uint64, 0, len(quantities))
				for id := range quantities {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				for _, shopProductID := range ids {
					stock, err := s.repo.LockStock(ctx, tx, shopProductID)
					if err != nil {
						return err
					}
					qty := quantities[shopProductID]
					before := stock.AvailableQty
					if err := s.repo.AddAvailableStock(ctx, tx, stock, qty); err != nil {
						return err
					}
					if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{ID: s.ids.Next(), ShopProductID: shopProductID, ShopID: row.ShopID, ProductID: products[shopProductID], ChangeType: "return", QuantityDelta: qty, BeforeAvailableQty: before, AfterAvailableQty: before + qty, SourceType: "after_sale_return", SourceID: &receiptID, IdempotencyKey: strPtr(key)}); err != nil {
						return err
					}
				}
			}
			if err := s.createRefundTask(ctx, tx, row, items, row.ApprovedAmount, now); err != nil {
				return err
			}
		}
		if err := s.repo.UpdateItemsDisposition(ctx, tx, row.ID, req.Disposition); err != nil {
			return err
		}
		ok, err := s.repo.UpdateCAS(ctx, tx, row.ID, req.Version, map[string]any{"status": target})
		if err != nil {
			return err
		}
		if !ok {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		if err := s.repo.CreateHistory(ctx, tx, s.history(row.ID, "merchant", actor.userID, "receive_return", strPtr(row.Status), strPtr(target), "", req.Remark)); err != nil {
			return err
		}
		if err := s.repo.CreateAudit(ctx, tx, s.audit(ctx, "merchant", actor.userID, "after_sale.receive_return", row.ID, row, receipt)); err != nil {
			return err
		}
		if err := s.repo.CreateOutbox(ctx, tx, s.outbox(ctx, "after_sale.return_received", row.ID, map[string]any{"after_sale_id": idString(row.ID), "receipt_id": idString(receiptID), "disposition": req.Disposition})); err != nil {
			return err
		}
		out = ReturnReceiptDTO{ID: idString(receipt.ID), ReceiptNo: receipt.ReceiptNo, AfterSaleID: idString(row.ID), Disposition: receipt.Disposition, SealedPackageIntact: receipt.SealedPackageIntact, GoodsIntact: receipt.GoodsIntact, ReceivedAt: now.Format(time.RFC3339Nano)}
		return s.idem.Succeed(ctx, tx, claims.AccountType, actor.userID, path, key, out)
	})
	return out, err
}

// ReserveReplacement 预留Replacement。
func (s *Service) ReserveReplacement(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ReplacementReq) (ReplacementDTO, error) {
	actor, err := storeActor(claims, "after_sale:create_replacement")
	if err != nil {
		return ReplacementDTO{}, err
	}
	afterSaleID, err := parseID(idRaw)
	if err != nil {
		return ReplacementDTO{}, problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
	}
	var out ReplacementDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actor.userID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return s.cached(ctx, tx, claims.AccountType, actor.userID, path, key, &out)
		}
		row, err := s.repo.Scoped(ctx, tx, afterSaleID, actor.merchantID, actor.shopIDs, true)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("AFTER_SALE_NOT_FOUND", "after-sale not found")
		}
		if err != nil {
			return err
		}
		if row.Version != req.Version {
			return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "after-sale was changed")
		}
		if row.Status != "replacement_processing" {
			return problem.Conflict("AFTER_SALE_STATUS_CONFLICT", "replacement is not allowed")
		}
		replacement, err := s.repo.Replacement(ctx, tx, row.ID, true)
		if err != nil {
			return err
		}
		if replacement.Status == "created" {
			var items []Item
			if json.Unmarshal(replacement.ItemsJSON, &items) != nil {
				return problem.Internal("invalid replacement items snapshot")
			}
			sort.Slice(items, func(i, j int) bool { return items[i].ShopProductID < items[j].ShopProductID })
			for _, item := range items {
				stock, err := s.repo.LockStock(ctx, tx, item.ShopProductID)
				if err != nil {
					return err
				}
				before := stock.AvailableQty
				ok, err := s.repo.ReserveReplacementStock(ctx, tx, stock, item.ApprovedQuantity)
				if err != nil {
					return err
				}
				if !ok {
					return problem.Conflict("STOCK_INSUFFICIENT", "replacement stock is insufficient")
				}
				if err := s.repo.CreateStockRecord(ctx, tx, StockRecord{ID: s.ids.Next(), ShopProductID: item.ShopProductID, ShopID: row.ShopID, ProductID: item.ProductID, ChangeType: "reserve", QuantityDelta: -item.ApprovedQuantity, BeforeAvailableQty: before, AfterAvailableQty: before - item.ApprovedQuantity, SourceType: "replacement", SourceID: &replacement.ID, IdempotencyKey: strPtr(key)}); err != nil {
					return err
				}
			}
			ok, err := s.repo.UpdateReplacement(ctx, tx, replacement.ID, replacement.Version, map[string]any{"status": "stock_reserved"})
			if err != nil {
				return err
			}
			if !ok {
				return problem.Conflict("AFTER_SALE_VERSION_CONFLICT", "replacement was changed")
			}
			replacement.Status = "stock_reserved"
			replacement.Version++
			if err := s.repo.CreateHistory(ctx, tx, s.history(row.ID, "merchant", actor.userID, "reserve_replacement", strPtr(row.Status), strPtr(row.Status), "", "")); err != nil {
				return err
			}
			if err := s.repo.CreateOutbox(ctx, tx, s.outbox(ctx, "replacement.stock_reserved", row.ID, map[string]any{"after_sale_id": idString(row.ID), "replacement_id": idString(replacement.ID)})); err != nil {
				return err
			}
		}
		out = ReplacementDTO{ID: idString(replacement.ID), ReplacementNo: replacement.ReplacementNo, AfterSaleID: idString(row.ID), Status: replacement.Status, Version: replacement.Version}
		return s.idem.Succeed(ctx, tx, claims.AccountType, actor.userID, path, key, out)
	})
	return out, err
}

// cached 返回缓存。
func (s *Service) cached(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, path, key string, out any) error {
	ok, e := s.idem.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if e != nil {
		return e
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
	}
	return nil
}

type evidenceClaims = evidencetoken.Claims

// evidence 返回evidence。
func (s *Service) evidence(afterID, ownerID uint64, tokens []string) ([]Evidence, error) {
	rows := make([]Evidence, 0, len(tokens))
	for _, token := range tokens {
		meta, err := evidencetoken.Verify(token, evidencetoken.Policy{
			Secret: s.cfg.AfterSale.EvidenceTokenSecret, Issuer: "jxe-upload", Subject: idString(ownerID),
			AllowedMedia: map[string]evidencetoken.MediaRule{
				"image/jpeg": {MaxBytes: 20 << 20}, "image/png": {MaxBytes: 20 << 20},
				"image/heic": {MaxBytes: 20 << 20}, "video/mp4": {MaxBytes: 100 << 20},
			},
			AllowedScanStatus: map[string]bool{"clean": true, "pending": true}, Now: s.now,
		})
		if err != nil {
			return nil, problem.InvalidArgument("AFTER_SALE_EVIDENCE_INVALID", "invalid or expired evidence token")
		}
		status := "verified"
		switch meta.ScanStatus {
		case "clean":
		case "pending":
			status = "quarantined"
		default:
			return nil, problem.InvalidArgument("AFTER_SALE_EVIDENCE_INVALID", "evidence scan status is not allowed")
		}
		rows = append(rows, Evidence{ID: s.ids.Next(), AfterSaleID: afterID, TokenID: meta.TokenID, ObjectKey: meta.ObjectKey, MimeType: meta.MimeType, SizeBytes: meta.SizeBytes, SHA256: meta.SHA256, Status: status, CreatedAt: s.now().UTC()})
	}
	return rows, nil
}

// history 返回history。
func (s *Service) history(id uint64, actorType string, actorID uint64, action string, from, to *string, reason, remark string) History {
	return History{ID: s.ids.Next(), AfterSaleID: id, ActorType: actorType, ActorID: actorID, Action: action, FromStatus: from, ToStatus: to, ReasonCode: optional(reason), Remark: optional(remark), RequestID: nil}
}

// audit 返回审计。
func (s *Service) audit(ctx context.Context, actorType string, actorID uint64, action string, id uint64, before, after any) AuditLog {
	return AuditLog{ID: s.ids.Next(), ActorType: actorType, ActorID: actorID, Action: action, ResourceType: "after_sale", ResourceID: id, BeforeData: jsonData(before), AfterData: jsonData(after), Result: "success", RequestID: requestctx.RequestIDPtr(ctx), IP: requestctx.IPPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx)}
}

// outbox 返回发件箱事件。
func (s *Service) outbox(ctx context.Context, event string, id uint64, payload any) OutboxEvent {
	return OutboxEvent{ID: s.ids.Next(), EventID: uuid.NewString(), EventType: event, AggregateType: "after_sale", AggregateID: id, Payload: jsonData(payload), Status: "pending", RequestID: requestctx.RequestIDPtr(ctx)}
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(v any) datatypes.JSON { b, _ := json.Marshal(v); return b }

// optional 返回optional。
func optional(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	x := strings.TrimSpace(v)
	return &x
}

// strPtr 返回str Ptr。
func strPtr(v string) *string { return &v }

// parseID 解析并校验字符串形式的 ID。
func parseID(v string) (uint64, error) {
	id, e := strconv.ParseUint(v, 10, 64)
	if e != nil || id == 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// idString 将数字 ID 转换为字符串。
func idString(v uint64) string { return strconv.FormatUint(v, 10) }

func optionalIDString(v *uint64) string {
	if v == nil {
		return ""
	}
	return idString(*v)
}

// customerActor 返回用户 Actor。
func customerActor(c *auth.Claims) (uint64, error) {
	if c == nil || c.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
	}
	id, e := parseID(c.CustomerID)
	if e != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
	}
	return id, nil
}

// hasPermission 判断是否存在权限。
func hasPermission(c *auth.Claims, p string) bool {
	for _, v := range c.Permissions {
		if v == p {
			return true
		}
	}
	return false
}

// adminActor 返回管理端 Actor。
func adminActor(c *auth.Claims, p string) (uint64, error) {
	if c == nil || c.AccountType != "admin" || !hasPermission(c, p) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	id, e := parseID(c.AdminUserID)
	if e != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	return id, nil
}

// storeActor 返回门店 Actor。
func storeActor(c *auth.Claims, p string) (storeIdentity, error) {
	if c == nil || c.AccountType != "merchant" {
		return storeIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "merchant account required")
	}
	if !hasPermission(c, p) {
		return storeIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	u, e := parseID(c.MerchantUserID)
	if e != nil {
		return storeIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "invalid merchant identity")
	}
	m, e := parseID(c.MerchantID)
	if e != nil {
		return storeIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "invalid merchant identity")
	}
	shops := make([]uint64, 0, len(c.AuthorizedShopIDs))
	for _, v := range c.AuthorizedShopIDs {
		id, e := parseID(v)
		if e != nil {
			return storeIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "invalid shop scope")
		}
		shops = append(shops, id)
	}
	if len(shops) == 0 {
		return storeIdentity{}, problem.Forbidden("PERM_FORBIDDEN", "no authorized shops")
	}
	return storeIdentity{u, m, shops}, nil
}

// rowsDTO 返回rows DTO。
func (s *Service) rowsDTO(rows []AfterSale) []DTO {
	out := make([]DTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.dto(r, nil, nil, nil))
	}
	return out
}

// dto 返回DTO。
func (s *Service) dto(r AfterSale, items []Item, ev []Evidence, h []History) DTO {
	d := DTO{ID: idString(r.ID), AfterSaleNo: r.AfterSaleNo, OrderID: idString(r.OrderID), CustomerID: idString(r.CustomerID), MerchantID: idString(r.MerchantID), ShopID: idString(r.ShopID), InitiatorType: r.InitiatorType, SourceType: r.SourceType, SourceID: optionalIDString(r.SourceID), Type: r.Type, RequestedResolution: r.RequestedResolution, Status: r.Status, RequestedAmount: r.RequestedAmount, ApprovedAmount: r.ApprovedAmount, RefundedAmount: r.RefundedAmount, CompensationAmount: r.CompensationAmount, IncludeDeliveryFee: r.IncludeDeliveryFee, Description: r.Description, Version: r.Version, SubmittedAt: r.SubmittedAt.Format(time.RFC3339), CreatedAt: r.CreatedAt.Format(time.RFC3339), UpdatedAt: r.UpdatedAt.Format(time.RFC3339)}
	if r.ApprovedResolution != nil {
		d.ApprovedResolution = *r.ApprovedResolution
	}
	if r.ReasonCode != nil {
		d.ReasonCode = *r.ReasonCode
	}
	for _, v := range items {
		d.Items = append(d.Items, ItemDTO{ID: idString(v.ID), OrderItemID: idString(v.OrderItemID), ShopProductID: idString(v.ShopProductID), ProductID: idString(v.ProductID), RequestedQuantity: v.RequestedQuantity, ApprovedQuantity: v.ApprovedQuantity, RequestedAmount: v.RequestedAmount, ApprovedAmount: v.ApprovedAmount, RefundedAmount: v.RefundedAmount, ReturnDisposition: v.ReturnDisposition})
	}
	for _, v := range ev {
		d.Evidence = append(d.Evidence, EvidenceDTO{ID: idString(v.ID), MimeType: v.MimeType, SizeBytes: v.SizeBytes, SHA256: v.SHA256, Status: v.Status, CreatedAt: v.CreatedAt.Format(time.RFC3339)})
	}
	for _, v := range h {
		x := HistoryDTO{Action: v.Action, ActorType: v.ActorType, CreatedAt: v.CreatedAt.Format(time.RFC3339)}
		if v.FromStatus != nil {
			x.FromStatus = *v.FromStatus
		}
		if v.ToStatus != nil {
			x.ToStatus = *v.ToStatus
		}
		if v.ReasonCode != nil {
			x.ReasonCode = *v.ReasonCode
		}
		if v.Remark != nil {
			x.Remark = *v.Remark
		}
		d.History = append(d.History, x)
	}
	return d
}
