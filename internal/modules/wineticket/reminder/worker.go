package reminder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	ExpiryReminderDays       = uint8(7)
	expiryReminderDays       = ExpiryReminderDays
	expiryReminderLeadTime   = 168 * time.Hour
	defaultReminderSendLease = 5 * time.Minute
)

type ExpiryReminderWorker struct {
	db               *gorm.DB
	reminderRepo     *reminderWorkerRepository
	ids              *snowflake.Generator
	provider         notification.Provider
	instance         string
	log              *slog.Logger
	now              func() time.Time
	batch            int
	interval         time.Duration
	remindersEnabled bool
	wechatEnabled    bool
	wechatAppID      string
	sendLease        time.Duration
}

func NewWorker(
	db *gorm.DB,
	ids *snowflake.Generator,
	provider notification.Provider,
	instance string,
	log *slog.Logger,
) *ExpiryReminderWorker {
	return NewExpiryReminderWorker(db, ids, provider, instance, log)
}

func NewExpiryReminderWorker(
	db *gorm.DB,
	ids *snowflake.Generator,
	provider notification.Provider,
	instance string,
	log *slog.Logger,
) *ExpiryReminderWorker {
	if provider == nil {
		provider = &notification.UnavailableProvider{}
	}
	if log == nil {
		log = slog.Default()
	}
	instance = strings.TrimSpace(instance)
	if instance == "" {
		instance = "wine-ticket-expiry-reminder"
	}
	return &ExpiryReminderWorker{
		db: db, reminderRepo: &reminderWorkerRepository{},
		ids: ids, provider: provider, instance: instance, log: log,
		now: time.Now, batch: 100, interval: time.Minute,
		remindersEnabled: true, wechatEnabled: true,
		sendLease: defaultReminderSendLease,
	}
}

func (w *ExpiryReminderWorker) WithNow(now func() time.Time) *ExpiryReminderWorker {
	if now != nil {
		w.now = now
	}
	return w
}

func (w *ExpiryReminderWorker) WithBatchSize(batch int) *ExpiryReminderWorker {
	if batch > 0 && batch <= 1000 {
		w.batch = batch
	}
	return w
}

func (w *ExpiryReminderWorker) WithInterval(interval time.Duration) *ExpiryReminderWorker {
	if interval > 0 {
		w.interval = interval
	}
	return w
}

// WithRemindersEnabled 允许客户提醒功能停止时，过期完整性循环继续运行。
// 过期并非可选的通知副作用：批次一旦存在，即使处于回滚或事故响应阶段，
// 也必须继续关闭到期的可用余额。
func (w *ExpiryReminderWorker) WithRemindersEnabled(enabled bool) *ExpiryReminderWorker {
	w.remindersEnabled = enabled
	return w
}

// WithWeChatEnabled 独立控制尽力而为的微信订阅消息通道。
// 它不会关闭站内信生成或批次过期；禁用时既不创建微信发送事实，
// 也不消费一次性授权。
func (w *ExpiryReminderWorker) WithWeChatEnabled(enabled bool) *ExpiryReminderWorker {
	w.wechatEnabled = enabled
	return w
}

// WithWeChatAppID 使服务商发送流程在同一个认领事务中
// 解析当前客户有效的小程序身份。OpenID 仅用于外部调用，
// 不会复制到提醒事实或日志中。
func (w *ExpiryReminderWorker) WithWeChatAppID(appID string) *ExpiryReminderWorker {
	w.wechatAppID = strings.TrimSpace(appID)
	return w
}

// WithSendLease 限制进行中的服务商调用时长。
// 当前持有者仍可能完成外部发送时，其他任务不得变更该提醒。
func (w *ExpiryReminderWorker) WithSendLease(lease time.Duration) *ExpiryReminderWorker {
	if lease > 0 {
		w.sendLease = lease
	}
	return w
}

func (w *ExpiryReminderWorker) Run(ctx context.Context) {
	w.runAndLog(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runAndLog(ctx)
		}
	}
}

func (w *ExpiryReminderWorker) runAndLog(ctx context.Context) {
	if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.log.Error(
			"wine ticket expiry reminder worker failed",
			slog.String("instance", w.instance), slog.Any("error", err),
		)
	}
}

// RunOnce 补齐提醒，并保证每条已认领服务商消息最多发送一次。
// 批次过期有意放在提醒子域之外。
func (w *ExpiryReminderWorker) RunOnce(ctx context.Context) error {
	if !w.remindersEnabled {
		return nil
	}
	if _, err := w.MaterializeRemindersOnce(ctx); err != nil {
		return err
	}
	_, err := w.DispatchRemindersOnce(ctx)
	return err
}

func (w *ExpiryReminderWorker) nowShanghai() time.Time {
	return core.NowShanghai(w.now)
}

func (w *ExpiryReminderWorker) MaterializeRemindersOnce(ctx context.Context) (int, error) {
	now := w.nowShanghai()
	candidates, err := w.reminderRepo.materializationCandidates(
		ctx,
		w.db,
		now,
		w.batch,
		w.wechatEnabled,
	)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, candidate := range candidates {
		count, processErr := w.materializeLotReminder(ctx, candidate, now)
		if processErr != nil {
			return created, processErr
		}
		created += count
	}
	return created, nil
}

func (w *ExpiryReminderWorker) materializeLotReminder(
	ctx context.Context, candidate core.Lot, now time.Time,
) (int, error) {
	created := 0
	err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, found, err := w.reminderRepo.lockLot(ctx, tx, candidate.ID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		scheduledAt := reminderScheduledAt(locked)
		_, eligibilityCode, err := w.reminderLotEligibility(
			ctx,
			tx,
			locked,
			candidate.OwnerCustomerID,
			candidate.ExpiresAt,
			now,
		)
		if err != nil {
			return err
		}
		if eligibilityCode != "" {
			// 已消费、已领取或已退款而耗尽的批次，在配送退回后可能再次可用。
			// 余额为零时不得提前消耗每次过期的去重键。
			if eligibilityCode == "NO_REMAINING_BOTTLES" {
				return nil
			}
			factLot := locked
			if eligibilityCode == "LOT_FACT_CHANGED" {
				factLot = candidate
			}
			return w.createSkippedReminderFacts(ctx, tx, factLot, now, eligibilityCode)
		}
		if scheduledAt.After(now) {
			return w.createSkippedReminderFacts(ctx, tx, locked, now, "LOT_NOT_REMINDABLE")
		}
		inboxInserted, err := w.createReminderFact(
			ctx, tx, locked, scheduledAt, "inbox", "sent", nil, &now,
		)
		if err != nil {
			return err
		}
		if inboxInserted {
			created++
			if err := w.createReminderInboxAndOutbox(ctx, tx, locked, now); err != nil {
				return err
			}
		}
		if w.wechatEnabled {
			wechatInserted, err := w.createReminderFact(
				ctx, tx, locked, scheduledAt, "wechat_subscription", "pending", nil, nil,
			)
			if err != nil {
				return err
			}
			if wechatInserted {
				created++
			}
		}
		return nil
	})
	return created, err
}

func reminderScheduledAt(lot core.Lot) time.Time {
	scheduledAt := lot.ExpiresAt.Add(-expiryReminderLeadTime)
	if lot.ExpiryChangedAt.After(scheduledAt) {
		scheduledAt = lot.ExpiryChangedAt
	}
	return scheduledAt.In(core.ShanghaiLocation).Truncate(time.Millisecond)
}

func (w *ExpiryReminderWorker) createSkippedReminderFacts(
	ctx context.Context,
	tx *gorm.DB,
	lot core.Lot,
	now time.Time,
	code string,
) error {
	scheduledAt := reminderScheduledAt(lot)
	if !scheduledAt.Before(lot.ExpiresAt) {
		return nil
	}
	channels := []string{"inbox"}
	if w.wechatEnabled {
		channels = append(channels, "wechat_subscription")
	}
	for _, channel := range channels {
		if _, err := w.createReminderFact(
			ctx, tx, lot, scheduledAt, channel, "skipped", &code, nil,
		); err != nil {
			return err
		}
	}
	return nil
}

func (w *ExpiryReminderWorker) createReminderFact(
	ctx context.Context,
	tx *gorm.DB,
	lot core.Lot,
	scheduledAt time.Time,
	channel, status string,
	lastErrorCode *string,
	sentAt *time.Time,
) (bool, error) {
	now := w.nowShanghai()
	row := Reminder{
		ID: w.ids.Next(), LotID: lot.ID, OwnerCustomerID: lot.OwnerCustomerID,
		ExpiresAt: lot.ExpiresAt, RemindDays: expiryReminderDays, Channel: channel,
		Status: status, LastErrorCode: lastErrorCode, ScheduledAt: scheduledAt,
		SentAt: sentAt, CreatedAt: now, UpdatedAt: now,
	}
	return w.reminderRepo.createReminderFact(ctx, tx, &row)
}

func (w *ExpiryReminderWorker) createReminderInboxAndOutbox(
	ctx context.Context, tx *gorm.DB, lot core.Lot, now time.Time,
) error {
	eventID := reminderEventID(lot.ID, lot.ExpiresAt)
	targetType := "wine_ticket_lot"
	message := notification.Message{
		ID: w.ids.Next(), CustomerID: lot.OwnerCustomerID, SourceEventID: eventID,
		Type: "wine_ticket.expiry_reminder", Title: "酒票即将到期",
		Summary:    "您的酒票将在 " + core.FormatShanghai(lot.ExpiresAt) + " 到期，请及时使用。",
		TargetType: &targetType, TargetID: &lot.ID, CreatedAt: now,
	}
	if err := w.reminderRepo.createInboxMessage(ctx, tx, &message); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"lot_id": core.IDString(lot.ID), "customer_id": core.IDString(lot.OwnerCustomerID),
		"expires_at": core.FormatShanghai(lot.ExpiresAt), "remind_days": expiryReminderDays,
	})
	return w.reminderRepo.createOutbox(ctx, tx, map[string]any{
		"id": w.ids.Next(), "event_id": eventID,
		"event_type": "wine_ticket.expiry_reminder.created", "event_version": 1,
		"spec_version": "1.0", "aggregate_type": "wine_ticket_lot",
		"aggregate_id": lot.ID, "producer": "wine-ticket",
		"payload": datatypes.JSON(payload), "status": "pending", "retry_count": 0,
		"created_at": now,
	})
}

func reminderEventID(lotID uint64, expiresAt time.Time) string {
	return fmt.Sprintf("wt-expiry-reminder:%d:%d", lotID, expiresAt.UnixMilli())
}

type reminderSendClaim struct {
	ReminderID        uint64
	ConsentID         uint64
	CustomerID        uint64
	Recipient         string
	LotID             uint64
	TemplateCode      string
	ProviderRequestID string
	LockOwner         string
	ExpiresAt         time.Time
	Payload           datatypes.JSON
}

func (w *ExpiryReminderWorker) DispatchRemindersOnce(ctx context.Context) (int, error) {
	if !w.wechatEnabled {
		return 0, nil
	}
	now := w.nowShanghai()
	rows, err := w.reminderRepo.dispatchCandidates(ctx, w.db, now, w.batch)
	if err != nil {
		return 0, err
	}
	dispatched := 0
	for _, row := range rows {
		claim, claimed, claimErr := w.claimReminderForSend(
			ctx,
			row.ID,
			row.LotID,
			now,
		)
		if claimErr != nil {
			return dispatched, claimErr
		}
		if !claimed {
			continue
		}
		if !w.nowShanghai().Before(claim.ExpiresAt) {
			if err := w.cancelReminderClaimBeforeSend(ctx, claim, "LOT_EXPIRED"); err != nil {
				return dispatched, err
			}
			continue
		}
		result, sendErr := w.provider.Send(ctx, notification.SendRequest{
			ProviderRequestID: claim.ProviderRequestID, TemplateID: claim.TemplateCode,
			Recipient: claim.Recipient, Payload: claim.Payload,
		})
		if err := w.finishReminderSend(ctx, claim, result, sendErr); err != nil {
			return dispatched, err
		}
		dispatched++
	}
	return dispatched, nil
}

func (w *ExpiryReminderWorker) claimReminderForSend(
	ctx context.Context,
	reminderID uint64,
	lotID uint64,
	now time.Time,
) (reminderSendClaim, bool, error) {
	var claim reminderSendClaim
	claimed := false
	err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 生成提醒事实前会先锁定批次。
		// 此处沿用批次 -> 提醒的顺序，避免并发生成和发送事务形成循环锁。
		lot, lotFound, err := w.reminderRepo.lockLot(ctx, tx, lotID)
		if err != nil {
			return err
		}
		reminder, found, err := w.reminderRepo.lockReminder(ctx, tx, reminderID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if reminder.Status != "pending" || reminder.Channel != "wechat_subscription" {
			return nil
		}
		if reminder.LotID != lotID {
			return fmt.Errorf(
				"reminder %d lot changed from %d to %d",
				reminder.ID,
				lotID,
				reminder.LotID,
			)
		}
		if reminder.Attempts > 0 || reminder.ProviderMessageID != nil {
			if reminder.LockedUntil != nil && reminder.LockedUntil.After(now) {
				return nil
			}
			code := "PROVIDER_RESULT_RECONCILIATION_REQUIRED"
			if err := w.reminderRepo.updatePendingReminder(
				ctx,
				tx,
				reminder.ID,
				map[string]any{
					"status":          "failed",
					"last_error_code": code,
					"locked_by":       nil,
					"locked_until":    nil,
					"updated_at":      now,
				},
			); err != nil {
				return err
			}
			return w.reminderRepo.updateSendingConsentsByReminder(
				ctx,
				tx,
				reminder.ID,
				map[string]any{"status": "unknown", "updated_at": now},
			)
		}
		if reminder.ScheduledAt.After(now) || !now.Before(reminder.ExpiresAt) {
			return w.reminderRepo.updatePendingReminder(
				ctx,
				tx,
				reminder.ID,
				map[string]any{
					"status":          "skipped",
					"last_error_code": "LOT_EXPIRED",
					"updated_at":      now,
				},
			)
		}

		if !lotFound {
			return w.reminderRepo.updatePendingReminder(
				ctx,
				tx,
				reminder.ID,
				map[string]any{
					"status":          "skipped",
					"last_error_code": "LOT_NOT_FOUND",
					"updated_at":      now,
				},
			)
		}
		remainingBottles, eligibilityCode, err := w.reminderLotEligibility(
			ctx,
			tx,
			lot,
			reminder.OwnerCustomerID,
			reminder.ExpiresAt,
			now,
		)
		if err != nil {
			return err
		}
		if eligibilityCode != "" {
			return w.reminderRepo.updatePendingReminder(
				ctx,
				tx,
				reminder.ID,
				map[string]any{
					"status":          "skipped",
					"last_error_code": eligibilityCode,
					"updated_at":      now,
				},
			)
		}
		productName, found, err := w.reminderRepo.productName(ctx, tx, lot.ProductID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(productName) == "" {
			return w.reminderRepo.updatePendingReminder(
				ctx,
				tx,
				reminder.ID,
				map[string]any{
					"status":          "skipped",
					"last_error_code": "PRODUCT_NOT_AVAILABLE",
					"updated_at":      now,
				},
			)
		}
		recipient := strconv.FormatUint(reminder.OwnerCustomerID, 10)
		if w.wechatAppID != "" {
			openID, identityCount, err := w.reminderRepo.activeMiniAppIdentity(
				ctx,
				tx,
				reminder.OwnerCustomerID,
				w.wechatAppID,
			)
			if err != nil {
				return err
			}
			recipient = strings.TrimSpace(openID)
			if identityCount != 1 || recipient == "" {
				return w.reminderRepo.updatePendingReminder(
					ctx,
					tx,
					reminder.ID,
					map[string]any{
						"status":          "skipped",
						"last_error_code": "OPENID_NOT_AVAILABLE",
						"updated_at":      now,
					},
				)
			}
		}
		if err := w.reminderRepo.expireAvailableConsents(
			ctx,
			tx,
			reminder.OwnerCustomerID,
			ExpiryScene,
			now,
		); err != nil {
			return err
		}
		consent, found, err := w.reminderRepo.lockAvailableConsent(
			ctx,
			tx,
			reminder.OwnerCustomerID,
			ExpiryScene,
			now,
		)
		if err != nil {
			return err
		}
		if !found {
			return w.reminderRepo.updatePendingReminder(
				ctx,
				tx,
				reminder.ID,
				map[string]any{
					"status":          "skipped",
					"last_error_code": "CONSENT_NOT_AVAILABLE",
					"updated_at":      now,
				},
			)
		}
		consentClaimed, err := w.reminderRepo.claimConsent(
			ctx,
			tx,
			consent.ID,
			reminder.ID,
			now,
		)
		if err != nil {
			return err
		}
		if !consentClaimed {
			return nil
		}
		providerRequestID := fmt.Sprintf("wt-reminder-%d", reminder.ID)
		lockOwner := reminderLockOwner(w.instance)
		lockedUntil := now.Add(w.sendLease)
		reminderClaimed, err := w.reminderRepo.claimReminder(
			ctx,
			tx,
			reminder.ID,
			providerRequestID,
			lockOwner,
			lockedUntil,
			now,
		)
		if err != nil {
			return err
		}
		if !reminderClaimed {
			return fmt.Errorf("reminder %d lost provider claim", reminder.ID)
		}
		payload, _ := json.Marshal(map[string]any{
			"product_short_name": truncateReminderProductName(productName, 20),
			"remaining_bottles":  remainingBottles,
			"expiry_date":        lot.ExpiresAt.In(core.ShanghaiLocation).Format("2006-01-02"),
		})
		claim = reminderSendClaim{
			ReminderID: reminder.ID, ConsentID: consent.ID,
			CustomerID: reminder.OwnerCustomerID, Recipient: recipient,
			LotID:        reminder.LotID,
			TemplateCode: consent.TemplateCode, ProviderRequestID: providerRequestID,
			LockOwner: lockOwner, ExpiresAt: reminder.ExpiresAt,
			Payload: datatypes.JSON(payload),
		}
		claimed = true
		return nil
	})
	return claim, claimed, err
}

func reminderLockOwner(instance string) string {
	instance = strings.TrimSpace(instance)
	runes := []rune(instance)
	if len(runes) <= 128 {
		return instance
	}
	return string(runes[:128])
}

func (w *ExpiryReminderWorker) remainingBottlesForReminder(
	ctx context.Context,
	tx *gorm.DB,
	lot core.Lot,
) (uint64, error) {
	held, err := w.reminderRepo.heldQuantity(ctx, tx, lot.ID)
	if err != nil {
		return 0, err
	}
	return uint64(lot.AvailableQuantity) + held, nil
}

func (w *ExpiryReminderWorker) reminderLotEligibility(
	ctx context.Context,
	tx *gorm.DB,
	lot core.Lot,
	expectedOwnerCustomerID uint64,
	expectedExpiresAt time.Time,
	now time.Time,
) (uint64, string, error) {
	if lot.OwnerCustomerID != expectedOwnerCustomerID ||
		!lot.ExpiresAt.Equal(expectedExpiresAt) ||
		(lot.Status != core.LotStatusActive && lot.Status != core.LotStatusDepleted) {
		return 0, "LOT_FACT_CHANGED", nil
	}
	if !now.Before(lot.ExpiresAt) {
		return 0, "LOT_EXPIRED", nil
	}
	remainingBottles, err := w.remainingBottlesForReminder(ctx, tx, lot)
	if err != nil {
		return 0, "", err
	}
	if remainingBottles == 0 {
		return 0, "NO_REMAINING_BOTTLES", nil
	}
	return remainingBottles, "", nil
}

func truncateReminderProductName(name string, maxRunes int) string {
	name = strings.TrimSpace(name)
	runes := []rune(name)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return name
}

func (w *ExpiryReminderWorker) cancelReminderClaimBeforeSend(
	ctx context.Context, claim reminderSendClaim, code string,
) error {
	now := w.nowShanghai()
	return w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updated, err := w.reminderRepo.updateOwnedReminder(
			ctx,
			tx,
			claim.ReminderID,
			claim.LockOwner,
			map[string]any{
				"status": "skipped", "last_error_code": code,
				"locked_by": nil, "locked_until": nil, "updated_at": now,
			},
		)
		if err != nil {
			return err
		}
		if !updated {
			return nil
		}
		return w.reminderRepo.updateSendingConsent(
			ctx,
			tx,
			claim.ConsentID,
			claim.ReminderID,
			map[string]any{
				"status": "available", "claimed_by_reminder_id": nil,
				"claimed_at": nil, "updated_at": now,
			},
		)
	})
}

func (w *ExpiryReminderWorker) finishReminderSend(
	ctx context.Context,
	claim reminderSendClaim,
	sendResult notification.SendResult,
	sendErr error,
) error {
	now := w.nowShanghai()
	if code, notAttempted := reminderProviderDidNotAttempt(sendErr); notAttempted {
		return w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			updated, err := w.reminderRepo.updateOwnedReminder(
				ctx,
				tx,
				claim.ReminderID,
				claim.LockOwner,
				map[string]any{
					"attempts": 0, "provider_message_id": nil,
					"last_error_code": code, "locked_by": nil,
					"locked_until": nil, "updated_at": now,
				},
			)
			if err != nil {
				return err
			}
			if !updated {
				return nil
			}
			return w.reminderRepo.updateSendingConsent(
				ctx,
				tx,
				claim.ConsentID,
				claim.ReminderID,
				map[string]any{
					"status": "available", "claimed_by_reminder_id": nil,
					"claimed_at": nil, "updated_at": now,
				},
			)
		})
	}
	reminderStatus, consentStatus, code := reminderProviderOutcome(sendResult, sendErr)
	providerID := strings.TrimSpace(sendResult.ProviderRequestID)
	if providerID == "" {
		providerID = claim.ProviderRequestID
	}
	return w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		reminderUpdates := map[string]any{
			"status": reminderStatus, "provider_message_id": providerID,
			"last_error_code": optionalTrimmedString(code),
			"locked_by":       nil, "locked_until": nil, "updated_at": now,
		}
		if reminderStatus == "sent" {
			reminderUpdates["sent_at"] = now
		}
		updated, err := w.reminderRepo.updateOwnedReminder(
			ctx,
			tx,
			claim.ReminderID,
			claim.LockOwner,
			reminderUpdates,
		)
		if err != nil {
			return err
		}
		if !updated {
			return nil
		}
		return w.reminderRepo.updateSendingConsent(
			ctx,
			tx,
			claim.ConsentID,
			claim.ReminderID,
			map[string]any{"status": consentStatus, "updated_at": now},
		)
	})
}

func reminderProviderDidNotAttempt(err error) (string, bool) {
	var providerErr *notification.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Unknown {
		return "", false
	}
	code := strings.TrimSpace(providerErr.Code)
	return code, strings.HasPrefix(strings.ToLower(code), "wechat_access_token_")
}

func reminderProviderOutcome(
	result notification.SendResult, err error,
) (reminderStatus, consentStatus, code string) {
	if err == nil && strings.EqualFold(strings.TrimSpace(result.Status), "succeeded") {
		return "sent", "consumed", ""
	}
	if err == nil {
		if strings.EqualFold(strings.TrimSpace(result.Status), "exhausted") {
			return "failed", "exhausted", "PROVIDER_QUOTA_EXHAUSTED"
		}
		return "failed", "unknown", "PROVIDER_RESULT_UNKNOWN"
	}
	var providerErr *notification.ProviderError
	if errors.As(err, &providerErr) {
		code = strings.TrimSpace(providerErr.Code)
		if code == "" {
			code = "PROVIDER_FAILURE"
		}
		lowerCode := strings.ToLower(code)
		if providerErr.Unknown || providerErr.Retryable {
			return "failed", "unknown", code
		}
		if strings.Contains(lowerCode, "quota") || strings.Contains(lowerCode, "exhaust") {
			return "failed", "exhausted", code
		}
		// 明确的服务商拒绝已经消耗或使该一次性授权失效，
		// 不得将其放回可用池。
		return "failed", "exhausted", code
	}
	return "failed", "unknown", "PROVIDER_RESULT_UNKNOWN"
}
