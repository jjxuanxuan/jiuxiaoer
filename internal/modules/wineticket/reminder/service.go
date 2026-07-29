package reminder

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	ExpiryScene         = "expiry_reminder"
	expiryReminderScene = ExpiryScene
)

// ReminderService 负责客户可见的订阅授权事实。
// 它不会直接调用服务商：只有用户操作可以创建授权，
// 也只有后台任务可以消费授权。
type ReminderService struct {
	repo *reminderRepository
	ids  *snowflake.Generator
	idem *idempotency.Store
	now  func() time.Time
}

func NewReminderService(db *gorm.DB, ids *snowflake.Generator) *ReminderService {
	return &ReminderService{
		repo: newReminderRepository(db), ids: ids, idem: idempotency.NewStore(db), now: time.Now,
	}
}

func (s *ReminderService) WithNow(now func() time.Time) *ReminderService {
	if now != nil {
		s.now = now
	}
	return s
}

func (s *ReminderService) LatestConsent(ctx context.Context, claims *auth.Claims, scene string) (NotificationConsentDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_notification_consent:view")
	if err != nil {
		return NotificationConsentDTO{}, err
	}
	if strings.TrimSpace(scene) != ExpiryScene {
		return NotificationConsentDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "scene must be expiry_reminder")
	}
	row, err := s.repo.latestConsent(ctx, customerID, ExpiryScene)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return NotificationConsentDTO{}, problem.NotFound(
			"WT_NOTIFICATION_CONSENT_NOT_FOUND", "notification subscription consent not found",
		)
	}
	if err != nil {
		return NotificationConsentDTO{}, err
	}
	return notificationConsentDTO(row, s.nowShanghai()), nil
}

func (s *ReminderService) RecordConsent(
	ctx context.Context,
	claims *auth.Claims,
	method, path, key string,
	req NotificationConsentCreateRequest,
) (NotificationConsentDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_notification_consent:create")
	if err != nil {
		return NotificationConsentDTO{}, err
	}
	req, status, err := normalizeNotificationConsentRequest(req)
	if err != nil {
		return NotificationConsentDTO{}, err
	}
	requestHash := idempotency.RequestHash(req)
	var out NotificationConsentDTO
	err = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.claimIdempotency(
			ctx, tx, claims.AccountType, customerID, method, path, key, requestHash,
		)
		if claimErr != nil {
			return claimErr
		}
		if !started {
			found, cachedErr := s.idem.CachedResponse(
				ctx, tx, claims.AccountType, customerID, path, key, &out,
			)
			if cachedErr != nil {
				return cachedErr
			}
			if !found {
				return problem.Conflict(
					"IDEMPOTENCY_IN_PROGRESS", "request with the same idempotency key is still processing",
				)
			}
			return nil
		}

		now := s.nowShanghai()
		row := NotificationSubscriptionConsent{
			ID: s.ids.Next(), CustomerID: customerID, Scene: req.Scene,
			TemplateCode: req.TemplateCode, ConsentResult: req.ConsentResult,
			ProviderReceipt: optionalTrimmedString(req.ProviderReceipt), Status: status,
			ConsentedAt: now, RequestID: requestctx.RequestIDPtr(ctx),
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.createConsent(ctx, tx, &row); err != nil {
			return err
		}
		out = notificationConsentDTO(row, now)
		return s.idem.Succeed(ctx, tx, claims.AccountType, customerID, path, key, out)
	})
	return out, err
}

func (s *ReminderService) claimIdempotency(
	ctx context.Context,
	tx *gorm.DB,
	actorType string,
	actorID uint64,
	method, path, key, requestHash string,
) (bool, error) {
	return s.idem.StartAt(
		ctx,
		tx,
		s.ids.Next(),
		actorType,
		actorID,
		method,
		path,
		key,
		requestHash,
		s.now(),
	)
}

func (s *ReminderService) nowShanghai() time.Time {
	return core.NowShanghai(s.now)
}

func normalizeNotificationConsentRequest(
	req NotificationConsentCreateRequest,
) (NotificationConsentCreateRequest, string, error) {
	req.Scene = strings.TrimSpace(req.Scene)
	req.TemplateCode = strings.TrimSpace(req.TemplateCode)
	req.ConsentResult = strings.TrimSpace(req.ConsentResult)
	req.ProviderReceipt = strings.TrimSpace(req.ProviderReceipt)
	if req.Scene != ExpiryScene {
		return req, "", problem.InvalidArgument("VALIDATION_FAILED", "scene must be expiry_reminder")
	}
	if len(req.TemplateCode) < 1 || len(req.TemplateCode) > 64 {
		return req, "", problem.InvalidArgument("VALIDATION_FAILED", "template_code must be between 1 and 64 characters")
	}
	if len(req.ProviderReceipt) > 128 {
		return req, "", problem.InvalidArgument("VALIDATION_FAILED", "provider_receipt must not exceed 128 characters")
	}
	switch req.ConsentResult {
	case "accepted":
		return req, "available", nil
	case "rejected":
		return req, "rejected", nil
	case "unknown":
		return req, "unknown", nil
	default:
		return req, "", problem.InvalidArgument(
			"VALIDATION_FAILED", "consent_result must be accepted, rejected, or unknown",
		)
	}
}

func notificationConsentDTO(row NotificationSubscriptionConsent, now time.Time) NotificationConsentDTO {
	status := row.Status
	if status == "available" && row.ExpiresAt != nil && !row.ExpiresAt.After(now) {
		status = "expired"
	}
	return NotificationConsentDTO{
		ConsentID: core.IDString(row.ID), Scene: row.Scene, TemplateCode: row.TemplateCode,
		ConsentResult: row.ConsentResult, Status: status, ConsentedAt: core.FormatShanghai(row.ConsentedAt),
	}
}

func customerIDWithPermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Unauthorized("AUTH_UNAUTHORIZED", "customer authentication required")
	}
	customerID, err := strconv.ParseUint(claims.CustomerID, 10, 64)
	if err != nil || customerID == 0 {
		return 0, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid customer identity")
	}
	for _, granted := range claims.Permissions {
		if granted == permission {
			return customerID, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

func optionalTrimmedString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
