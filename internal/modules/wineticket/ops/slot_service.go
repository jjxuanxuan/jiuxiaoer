package ops

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

var slotAdminClockPattern = regexp.MustCompile(
	`^([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]$`,
)

type SlotAdminService struct {
	core *serviceCore
	repo *slotAdminRepository
	now  func() time.Time
}

func NewSlotAdminService(core *Service) *SlotAdminService {
	service := &SlotAdminService{}
	if core != nil && core.serviceCore != nil && core.repo != nil {
		service.core = core.serviceCore
		service.repo = newSlotAdminRepository(core.repo.DB())
		service.now = core.now
	}
	return service
}

func (s *SlotAdminService) WithSlotAdminClock(clock func() time.Time) *SlotAdminService {
	if clock != nil {
		s.now = clock
	}
	return s
}

func (s *SlotAdminService) List(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	shopIDRaw string,
	serviceDateRaw string,
) ([]SlotAdminDTO, string, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, "", err
	}
	if _, err := adminIDWithPermission(claims, "wine_ticket_slot:list"); err != nil {
		return nil, "", err
	}
	authorizedShops, err := slotAdminAuthorizedShops(claims)
	if err != nil {
		return nil, "", err
	}
	filter := slotAdminListFilter{AuthorizedShops: authorizedShops}
	if shopIDRaw != "" {
		shopID, parseErr := parseExternalID(
			shopIDRaw,
			"shop_id",
		)
		if parseErr != nil {
			return nil, "", parseErr
		}
		if !slotAdminShopAuthorized(authorizedShops, shopID) {
			return nil, "", problem.Forbidden(
				"PERM_FORBIDDEN",
				"shop is outside the administrator scope",
			)
		}
		filter.ShopID = &shopID
	}
	if serviceDateRaw != "" {
		serviceDate, parseErr := parseSlotAdminServiceDate(serviceDateRaw)
		if parseErr != nil {
			return nil, "", parseErr
		}
		filter.ServiceDate = &serviceDate
	}

	rows, err := s.repo.list(ctx, query, filter)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(
			query,
			idString(rows[len(rows)-1].ID),
		)
	}
	now := s.nowShanghai()
	items := make([]SlotAdminDTO, 0, len(rows))
	for _, row := range rows {
		item, dtoErr := slotAdminDTO(row, now)
		if dtoErr != nil {
			return nil, "", dtoErr
		}
		items = append(items, item)
	}
	return items, next, nil
}

func (s *SlotAdminService) Create(
	ctx context.Context,
	claims *auth.Claims,
	method string,
	path string,
	key string,
	request SlotAdminCreateRequest,
) (response SlotAdminDTO, resultErr error) {
	if err := s.requireConfigured(); err != nil {
		return SlotAdminDTO{}, err
	}
	actorID, err := adminIDWithPermission(claims, "wine_ticket_slot:create")
	if err != nil {
		return SlotAdminDTO{}, err
	}
	input, err := normalizeSlotAdminCreate(request)
	if err != nil {
		return SlotAdminDTO{}, err
	}
	authorizedShops, err := slotAdminAuthorizedShops(claims)
	if err != nil {
		return SlotAdminDTO{}, err
	}
	if !slotAdminShopAuthorized(authorizedShops, input.ShopID) {
		return SlotAdminDTO{}, problem.Forbidden(
			"PERM_FORBIDDEN",
			"shop is outside the administrator scope",
		)
	}
	requestHash := idempotency.ResourceRequestHash(
		"wine_ticket.delivery_time_slot.create",
		input.ShopID,
		request,
	)

	resultErr = s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.core.claimIdempotency(
			ctx,
			tx,
			claims.AccountType,
			actorID,
			method,
			path,
			key,
			requestHash,
		)
		if claimErr != nil {
			return claimErr
		}
		if !started {
			return s.core.cachedResponse(
				ctx,
				tx,
				claims.AccountType,
				actorID,
				path,
				key,
				&response,
			)
		}

		shop, merchant, validationErr := s.lockAndValidateShop(
			ctx,
			tx,
			input.ShopID,
		)
		if validationErr != nil {
			return validationErr
		}
		now := s.nowShanghai()
		if !input.StartAt.After(now) || !input.CutoffAt.After(now) {
			return problem.InvalidArgument(
				"VALIDATION_FAILED",
				"delivery window and cutoff must be in the future",
			)
		}
		openSlots, lockErr := s.repo.lockOpenSlots(
			ctx,
			tx,
			input.ShopID,
			input.ServiceDate,
			0,
		)
		if lockErr != nil {
			return lockErr
		}
		if slotAdminOverlaps(openSlots, input.StartTime, input.EndTime) {
			return problem.Conflict(
				"WT_CONCURRENT_MODIFICATION",
				"delivery time slot overlaps an open window for this shop and date",
			)
		}

		slotID := s.core.ids.Next()
		row := redemption.DeliveryTimeSlot{
			ID: slotID, ShopID: input.ShopID,
			ServiceDate: input.ServiceDate,
			StartTime:   input.StartTime, EndTime: input.EndTime,
			CutoffAt:       input.CutoffAt,
			CapacityOrders: input.CapacityOrders, ReservedOrders: 0,
			Status: DeliveryTimeSlotStatusOpen, Version: 1,
			CreatedAt: now, UpdatedAt: now,
			CreatedBy: uint64Ptr(actorID), UpdatedBy: uint64Ptr(actorID),
		}
		if err := s.repo.create(ctx, tx, &row); err != nil {
			return slotAdminWriteError(err)
		}
		response, err = slotAdminDTO(
			slotAdminRecord{
				DeliveryTimeSlot: row,
				ShopName:         shop.Name,
				MerchantID:       merchant.ID,
				MerchantName:     merchant.Name,
			},
			now,
		)
		if err != nil {
			return err
		}
		if err := s.createAudit(
			ctx,
			tx,
			actorID,
			"wine_ticket.delivery_time_slot.create",
			nil,
			&response,
		); err != nil {
			return err
		}
		if err := s.createOutbox(
			ctx,
			tx,
			"create",
			response,
		); err != nil {
			return err
		}
		return s.core.idStore.Succeed(
			ctx,
			tx,
			claims.AccountType,
			actorID,
			path,
			key,
			response,
		)
	})
	return response, resultErr
}

func (s *SlotAdminService) Update(
	ctx context.Context,
	claims *auth.Claims,
	method string,
	path string,
	key string,
	slotIDRaw string,
	request SlotAdminUpdateRequest,
) (response SlotAdminDTO, resultErr error) {
	if err := s.requireConfigured(); err != nil {
		return SlotAdminDTO{}, err
	}
	actorID, err := adminIDWithPermission(claims, "wine_ticket_slot:update")
	if err != nil {
		return SlotAdminDTO{}, err
	}
	slotID, err := parseExternalID(slotIDRaw, "slot_id")
	if err != nil {
		return SlotAdminDTO{}, err
	}
	input, err := normalizeSlotAdminUpdate(request)
	if err != nil {
		return SlotAdminDTO{}, err
	}
	authorizedShops, err := slotAdminAuthorizedShops(claims)
	if err != nil {
		return SlotAdminDTO{}, err
	}
	requestHash := idempotency.ResourceRequestHash(
		"wine_ticket.delivery_time_slot.update",
		slotID,
		request,
	)

	resultErr = s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.core.claimIdempotency(
			ctx,
			tx,
			claims.AccountType,
			actorID,
			method,
			path,
			key,
			requestHash,
		)
		if claimErr != nil {
			return claimErr
		}
		if !started {
			return s.core.cachedResponse(
				ctx,
				tx,
				claims.AccountType,
				actorID,
				path,
				key,
				&response,
			)
		}

		lookup, lookupErr := s.repo.slotByID(
			ctx,
			tx,
			slotID,
			authorizedShops,
		)
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return problem.NotFound(
				"WT_SLOT_NOT_FOUND",
				"delivery time slot not found",
			)
		}
		if lookupErr != nil {
			return lookupErr
		}
		shop, merchant, validationErr := s.lockAndValidateShop(
			ctx,
			tx,
			lookup.ShopID,
		)
		if validationErr != nil {
			return validationErr
		}
		row, lockErr := s.repo.lockSlot(
			ctx,
			tx,
			slotID,
			authorizedShops,
		)
		if errors.Is(lockErr, gorm.ErrRecordNotFound) {
			return problem.NotFound(
				"WT_SLOT_NOT_FOUND",
				"delivery time slot not found",
			)
		}
		if lockErr != nil {
			return lockErr
		}
		if row.ShopID != shop.ID || row.Version != input.ExpectedVersion {
			return problem.Conflict(
				"WT_CONCURRENT_MODIFICATION",
				"delivery time slot changed concurrently",
			)
		}
		if input.CapacityOrders < row.ReservedOrders {
			return problem.Conflict(
				"WT_CONCURRENT_MODIFICATION",
				"capacity cannot be lower than existing reservations",
			)
		}

		cutoffAt := row.CutoffAt.In(shanghaiLocation).Truncate(time.Millisecond)
		if input.CutoffAt != nil {
			if row.ReservedOrders != 0 &&
				!sameMillisecond(cutoffAt, *input.CutoffAt) {
				return problem.Conflict(
					"WT_CONCURRENT_MODIFICATION",
					"cutoff cannot change after the slot has reservations",
				)
			}
			cutoffAt = *input.CutoffAt
		}
		startAt, _, windowErr := redemptionSlotWindow(
			row.ServiceDate,
			row.StartTime,
			row.EndTime,
		)
		if windowErr != nil || !cutoffAt.Before(startAt) {
			return problem.InvalidArgument(
				"VALIDATION_FAILED",
				"cutoff_at must be earlier than the delivery window",
			)
		}
		now := s.nowShanghai()
		if input.Status == DeliveryTimeSlotStatusOpen {
			if row.Status != DeliveryTimeSlotStatusOpen &&
				input.CapacityOrders <= row.ReservedOrders {
				return problem.Conflict(
					"WT_CONCURRENT_MODIFICATION",
					"reopened delivery time slot must have remaining capacity",
				)
			}
			if !startAt.After(now) || !cutoffAt.After(now) {
				return problem.Conflict(
					"WT_CONCURRENT_MODIFICATION",
					"a past delivery window or cutoff cannot be opened",
				)
			}
			openSlots, openErr := s.repo.lockOpenSlots(
				ctx,
				tx,
				row.ShopID,
				row.ServiceDate,
				row.ID,
			)
			if openErr != nil {
				return openErr
			}
			if slotAdminOverlaps(openSlots, row.StartTime, row.EndTime) {
				return problem.Conflict(
					"WT_CONCURRENT_MODIFICATION",
					"delivery time slot overlaps an open window for this shop and date",
				)
			}
		}

		before, err := slotAdminDTO(
			slotAdminRecord{
				DeliveryTimeSlot: row,
				ShopName:         shop.Name,
				MerchantID:       merchant.ID,
				MerchantName:     merchant.Name,
			},
			now,
		)
		if err != nil {
			return err
		}
		if err := s.repo.updateVersioned(
			ctx,
			tx,
			row,
			map[string]any{
				"capacity_orders": input.CapacityOrders,
				"status":          input.Status,
				"cutoff_at":       cutoffAt,
				"updated_by":      actorID,
				"updated_at":      now,
				"version":         gorm.Expr("version + 1"),
			},
		); err != nil {
			return slotAdminWriteError(err)
		}
		row.CapacityOrders = input.CapacityOrders
		row.Status = input.Status
		row.CutoffAt = cutoffAt
		row.UpdatedAt = now
		row.UpdatedBy = uint64Ptr(actorID)
		row.Version++
		response, err = slotAdminDTO(
			slotAdminRecord{
				DeliveryTimeSlot: row,
				ShopName:         shop.Name,
				MerchantID:       merchant.ID,
				MerchantName:     merchant.Name,
			},
			now,
		)
		if err != nil {
			return err
		}
		if err := s.createAudit(
			ctx,
			tx,
			actorID,
			"wine_ticket.delivery_time_slot.update",
			&before,
			&response,
		); err != nil {
			return err
		}
		if err := s.createOutbox(
			ctx,
			tx,
			"update",
			response,
		); err != nil {
			return err
		}
		return s.core.idStore.Succeed(
			ctx,
			tx,
			claims.AccountType,
			actorID,
			path,
			key,
			response,
		)
	})
	return response, resultErr
}

func (s *SlotAdminService) requireConfigured() error {
	if s == nil || s.core == nil || s.repo == nil ||
		s.core.ids == nil || s.core.idStore == nil {
		return problem.Internal("delivery time slot administration is not configured")
	}
	return nil
}

func (s *SlotAdminService) nowShanghai() time.Time {
	if s.now == nil {
		return time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	}
	return s.now().In(shanghaiLocation).Truncate(time.Millisecond)
}

func (s *SlotAdminService) lockAndValidateShop(
	ctx context.Context,
	tx *gorm.DB,
	shopID uint64,
) (slotAdminShop, slotAdminMerchant, error) {
	shop, err := s.repo.lockShop(ctx, tx, shopID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return slotAdminShop{}, slotAdminMerchant{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"shop_id does not reference an active fulfillment shop",
		)
	}
	if err != nil {
		return slotAdminShop{}, slotAdminMerchant{}, err
	}
	merchant, err := s.repo.merchant(ctx, tx, shop.MerchantID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return slotAdminShop{}, slotAdminMerchant{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"shop merchant is unavailable",
		)
	}
	if err != nil {
		return slotAdminShop{}, slotAdminMerchant{}, err
	}
	if shop.Status != "active" ||
		!validSlotAdminCityCode(shop.CityCode) ||
		merchant.Status != "active" ||
		merchant.ReviewStatus != "approved" ||
		strings.TrimSpace(merchant.Name) == "" {
		return slotAdminShop{}, slotAdminMerchant{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"shop_id does not reference an active fulfillment shop",
		)
	}
	return shop, merchant, nil
}

func normalizeSlotAdminCreate(
	request SlotAdminCreateRequest,
) (slotAdminCreateInput, error) {
	shopID, err := parseExternalID(request.ShopID, "shop_id")
	if err != nil {
		return slotAdminCreateInput{}, err
	}
	serviceDate, err := parseSlotAdminServiceDate(request.ServiceDate)
	if err != nil {
		return slotAdminCreateInput{}, err
	}
	startClock, err := parseSlotAdminClock(request.StartTime, "start_time")
	if err != nil {
		return slotAdminCreateInput{}, err
	}
	endClock, err := parseSlotAdminClock(request.EndTime, "end_time")
	if err != nil {
		return slotAdminCreateInput{}, err
	}
	startAt, endAt, err := redemptionSlotWindow(
		serviceDate,
		startClock,
		endClock,
	)
	if err != nil {
		return slotAdminCreateInput{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"start_time must be earlier than end_time",
		)
	}
	cutoffAt, err := parseSlotAdminDateTime(request.CutoffAt, "cutoff_at")
	if err != nil {
		return slotAdminCreateInput{}, err
	}
	if !cutoffAt.Before(startAt) {
		return slotAdminCreateInput{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"cutoff_at must be earlier than the delivery window",
		)
	}
	if request.CapacityOrders == 0 ||
		uint64(request.CapacityOrders) > slotAdminMaxUint32 {
		return slotAdminCreateInput{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"capacity_orders must be between 1 and 4294967295",
		)
	}
	return slotAdminCreateInput{
		ShopID: shopID, ServiceDate: serviceDate,
		StartTime: startClock, EndTime: endClock,
		StartAt: startAt, EndAt: endAt,
		CutoffAt: cutoffAt, CapacityOrders: request.CapacityOrders,
	}, nil
}

func normalizeSlotAdminUpdate(
	request SlotAdminUpdateRequest,
) (slotAdminUpdateInput, error) {
	status := request.Status
	if status != DeliveryTimeSlotStatusOpen &&
		status != DeliveryTimeSlotStatusClosed {
		return slotAdminUpdateInput{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"status must be open or closed",
		)
	}
	if request.CapacityOrders == 0 ||
		uint64(request.CapacityOrders) > slotAdminMaxUint32 ||
		request.ExpectedVersion == 0 ||
		uint64(request.ExpectedVersion) > slotAdminMaxUint32 {
		return slotAdminUpdateInput{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"capacity_orders and expected_version must be between 1 and 4294967295",
		)
	}
	var cutoffAt *time.Time
	if request.CutoffAt != nil {
		parsed, err := parseSlotAdminDateTime(*request.CutoffAt, "cutoff_at")
		if err != nil {
			return slotAdminUpdateInput{}, err
		}
		cutoffAt = &parsed
	}
	return slotAdminUpdateInput{
		CapacityOrders:  request.CapacityOrders,
		Status:          status,
		CutoffAt:        cutoffAt,
		ExpectedVersion: request.ExpectedVersion,
	}, nil
}

func parseSlotAdminServiceDate(raw string) (time.Time, error) {
	value := raw
	parsed, err := time.ParseInLocation("2006-01-02", value, shanghaiLocation)
	if err != nil || parsed.Format("2006-01-02") != value {
		return time.Time{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"service_date must use YYYY-MM-DD",
		)
	}
	return parsed, nil
}

func parseSlotAdminClock(raw string, field string) (string, error) {
	value := raw
	if !slotAdminClockPattern.MatchString(value) {
		return "", problem.InvalidArgument(
			"VALIDATION_FAILED",
			field+" must use HH:MM:SS",
		)
	}
	if _, err := time.Parse("15:04:05", value); err != nil {
		return "", problem.InvalidArgument(
			"VALIDATION_FAILED",
			field+" must use HH:MM:SS",
		)
	}
	return value, nil
}

func parseSlotAdminDateTime(raw string, field string) (time.Time, error) {
	value := raw
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			field+" must be an RFC3339 date-time with an explicit timezone",
		)
	}
	return parsed.In(shanghaiLocation).Truncate(time.Millisecond), nil
}

func slotAdminAuthorizedShops(claims *auth.Claims) ([]uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return nil, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	if auth.AdminRoleHasGlobalShopScope(claims.RoleCode) {
		return nil, nil
	}
	if len(claims.AuthorizedShopIDs) == 0 {
		return nil, problem.Forbidden(
			"PERM_FORBIDDEN",
			"administrator has no authorized shop scope",
		)
	}
	seen := make(map[uint64]struct{}, len(claims.AuthorizedShopIDs))
	ids := make([]uint64, 0, len(claims.AuthorizedShopIDs))
	for _, raw := range claims.AuthorizedShopIDs {
		id, err := parseExternalID(raw, "authorized_shop_id")
		if err != nil {
			return nil, problem.Forbidden(
				"PERM_FORBIDDEN",
				"administrator shop scope is invalid",
			)
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids, nil
}

func slotAdminShopAuthorized(scope []uint64, shopID uint64) bool {
	if len(scope) == 0 {
		return true
	}
	index := sort.Search(
		len(scope),
		func(index int) bool { return scope[index] >= shopID },
	)
	return index < len(scope) && scope[index] == shopID
}

func validSlotAdminCityCode(value *string) bool {
	if value == nil {
		return false
	}
	code := strings.TrimSpace(*value)
	if len(code) != 6 {
		return false
	}
	for _, char := range code {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func slotAdminOverlaps(
	rows []redemption.DeliveryTimeSlot,
	startClock string,
	endClock string,
) bool {
	for _, row := range rows {
		existingStart, err := normalizeRedemptionClock(row.StartTime)
		if err != nil {
			return true
		}
		existingEnd, err := normalizeRedemptionClock(row.EndTime)
		if err != nil {
			return true
		}
		if startClock < existingEnd && endClock > existingStart {
			return true
		}
	}
	return false
}

func slotAdminDTO(row slotAdminRecord, now time.Time) (SlotAdminDTO, error) {
	startAt, endAt, err := redemptionSlotWindow(
		row.ServiceDate,
		row.StartTime,
		row.EndTime,
	)
	if err != nil {
		return SlotAdminDTO{}, problem.Internal(
			"delivery time slot contains an invalid window",
		)
	}
	startClock, err := normalizeRedemptionClock(row.StartTime)
	if err != nil {
		return SlotAdminDTO{}, problem.Internal(
			"delivery time slot contains an invalid start time",
		)
	}
	endClock, err := normalizeRedemptionClock(row.EndTime)
	if err != nil {
		return SlotAdminDTO{}, problem.Internal(
			"delivery time slot contains an invalid end time",
		)
	}
	availability := DeliveryTimeSlotStatusOpen
	switch {
	case row.Status != DeliveryTimeSlotStatusOpen:
		availability = DeliveryTimeSlotStatusClosed
	case !row.CutoffAt.After(now):
		availability = "cutoff"
	case row.ReservedOrders >= row.CapacityOrders:
		availability = "full"
	}
	remaining := uint(0)
	if row.CapacityOrders > row.ReservedOrders {
		remaining = row.CapacityOrders - row.ReservedOrders
	}
	return SlotAdminDTO{
		SlotID: idString(row.ID), ShopID: idString(row.ShopID),
		ShopName:                  row.ShopName,
		IssuerMerchantID:          idString(row.MerchantID),
		IssuerMerchantDisplayName: row.MerchantName,
		ScheduledStartAt:          formatShanghai(startAt),
		ScheduledEndAt:            formatShanghai(endAt),
		CutoffAt:                  formatShanghai(row.CutoffAt),
		AvailabilityStatus:        availability,
		RemainingCapacity:         remaining, Version: row.Version,
		ServiceDate: row.ServiceDate.Format("2006-01-02"),
		StartTime:   startClock, EndTime: endClock,
		CapacityOrders: row.CapacityOrders,
		ReservedOrders: row.ReservedOrders, Status: row.Status,
		CreatedAt: formatShanghai(row.CreatedAt),
		UpdatedAt: formatShanghai(row.UpdatedAt),
	}, nil
}

func (s *SlotAdminService) createAudit(
	ctx context.Context,
	tx *gorm.DB,
	actorID uint64,
	action string,
	before *SlotAdminDTO,
	after *SlotAdminDTO,
) error {
	var beforeData, afterData datatypes.JSON
	var beforeStatus, afterStatus *string
	var resourceID, shopID uint64
	var version uint64
	if before != nil {
		beforeData = jsonData(before)
		value := before.Status
		beforeStatus = &value
		resourceID, _ = parseExternalID(before.SlotID, "slot_id")
		shopID, _ = parseExternalID(before.ShopID, "shop_id")
	}
	if after != nil {
		afterData = jsonData(after)
		value := after.Status
		afterStatus = &value
		resourceID, _ = parseExternalID(after.SlotID, "slot_id")
		shopID, _ = parseExternalID(after.ShopID, "shop_id")
		version = uint64(after.Version)
	}
	values := map[string]any{
		"id": s.core.ids.Next(), "actor_type": "admin", "actor_id": actorID,
		"action": action, "resource_type": "wine_ticket_delivery_time_slot",
		"resource_id": resourceID, "shop_id": shopID,
		"before_data": beforeData, "after_data": afterData,
		"result": "success", "before_status": beforeStatus,
		"after_status": afterStatus, "version": version,
		"request_id": requestctx.RequestIDPtr(ctx),
		"ip_hash":    requestctx.IPHashPtr(ctx),
		"user_agent": requestctx.UserAgentPtr(ctx),
	}
	if accountID := requestctx.AccountID(ctx); accountID != 0 {
		values["account_id"] = accountID
	}
	return s.core.repo.CreateAudit(ctx, tx, values)
}

func (s *SlotAdminService) createOutbox(
	ctx context.Context,
	tx *gorm.DB,
	action string,
	item SlotAdminDTO,
) error {
	slotID, err := parseExternalID(item.SlotID, "slot_id")
	if err != nil {
		return problem.Internal("delivery time slot outbox id is invalid")
	}
	return s.core.createWineTicketOutbox(
		ctx,
		tx,
		slotAdminChangedEvent,
		"delivery_time_slot",
		slotID,
		map[string]any{
			"slot_id": item.SlotID, "shop_id": item.ShopID,
			"action": action, "status": item.Status,
			"service_date": item.ServiceDate, "version": item.Version,
		},
	)
}
