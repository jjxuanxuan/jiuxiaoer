package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/mq"
)

type MQHandler struct {
	service *Service
}

var _ mq.ReliableAfterCommitConsumerHandler = (*MQHandler)(nil)

// NewMQHandler 创建并初始化消息队列 Handler。
func NewMQHandler(service *Service) *MQHandler { return &MQHandler{service: service} }

type realtimeTarget struct {
	riderID       uint64
	clientEvent   string
	aggregateType string
	aggregateID   uint64
	payload       map[string]any
	soundKey      string
	expiresAt     time.Time
}

// Handle 处理消费者结果请求。
func (h *MQHandler) Handle(ctx context.Context, tx *gorm.DB, envelope mq.EventEnvelope) (mq.ConsumerResult, error) {
	if h == nil || h.service == nil || h.service.ids == nil {
		return mq.ConsumerResult{}, mq.TemporaryConsumerError("REALTIME_UNAVAILABLE", "realtime service is unavailable", nil)
	}
	if !h.service.cfg.Realtime.Enabled {
		return mq.ConsumerResult{}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return mq.ConsumerResult{}, mq.TerminalConsumerError("REALTIME_PAYLOAD_INVALID", "realtime event payload is invalid", err)
	}
	if envelope.EventType == "order.paid" {
		orderID, err := idFromPayload(payload, "order_id")
		if err != nil {
			return mq.ConsumerResult{}, mq.TerminalConsumerError("REALTIME_ORDER_ID_INVALID", "paid order id is invalid", err)
		}
		if _, _, err := merchantPaidOrderRecipients(ctx, tx, orderID); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, errMerchantPaidOrderInvalid) {
				return mq.ConsumerResult{}, mq.TerminalConsumerError("REALTIME_PAID_ORDER_INVALID", "paid order fact is unavailable", err)
			}
			return mq.ConsumerResult{}, mq.TemporaryConsumerError("REALTIME_RECIPIENT_LOOKUP_FAILED", "realtime recipient lookup failed", err)
		}
		// mq_consumer_receipts is the durable event_id de-duplication point.
		// Merchant WS events themselves are intentionally transient because the
		// scoped store order list is the reconnect source of truth.
		return mq.ConsumerResult{RefType: "merchant_paid_order", RefID: orderID}, nil
	}
	targets, err := h.targets(ctx, tx, envelope, payload)
	if err != nil {
		var consumerError *mq.ConsumerError
		if errors.As(err, &consumerError) {
			return mq.ConsumerResult{}, err
		}
		return mq.ConsumerResult{}, mq.TemporaryConsumerError("REALTIME_RECIPIENT_LOOKUP_FAILED", "realtime recipient lookup failed", err)
	}
	if len(targets) == 0 {
		return mq.ConsumerResult{}, nil
	}
	now := time.Now().UTC()
	rows := make([]Delivery, 0, len(targets))
	seen := make(map[string]bool)
	for _, target := range targets {
		if target.riderID == 0 || target.clientEvent == "" || target.aggregateID == 0 {
			return mq.ConsumerResult{}, mq.TerminalConsumerError("REALTIME_TARGET_INVALID", "realtime target is invalid", nil)
		}
		key := strconv.FormatUint(target.riderID, 10) + "\x00" + target.clientEvent
		if seen[key] {
			continue
		}
		seen[key] = true
		encoded, err := json.Marshal(target.payload)
		if err != nil {
			return mq.ConsumerResult{}, err
		}
		expiresAt := target.expiresAt
		if expiresAt.IsZero() {
			expiresAt = now.Add(24 * time.Hour)
		}
		var soundKey *string
		if target.soundKey != "" {
			value := target.soundKey
			soundKey = &value
		}
		relayStatus := relayPending
		if !expiresAt.After(now) {
			relayStatus = relayExpired
		}
		rows = append(rows, Delivery{
			ID: h.service.ids.Next(), SourceEventID: envelope.EventID, SourceEventType: envelope.EventType,
			ClientEventType: target.clientEvent, RecipientType: recipientRider, RecipientID: target.riderID,
			AggregateType: target.aggregateType, AggregateID: target.aggregateID,
			PayloadSnapshot: datatypes.JSON(encoded), SoundKey: soundKey, OccurredAt: envelope.OccurredAt.UTC(),
			ExpiresAt: expiresAt.UTC(), RelayStatus: relayStatus, NextRelayAt: now,
		})
	}
	if len(rows) == 0 {
		return mq.ConsumerResult{}, nil
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, 200).Error; err != nil {
		return mq.ConsumerResult{}, err
	}
	var first Delivery
	if err := tx.WithContext(ctx).Where("source_event_id=?", envelope.EventID).Order("id").First(&first).Error; err != nil {
		return mq.ConsumerResult{}, err
	}
	return mq.ConsumerResult{RefType: "realtime_delivery", RefID: first.ID}, nil
}

// AfterCommit 返回售后 Commit。
func (h *MQHandler) AfterCommit(ctx context.Context, envelope mq.EventEnvelope, _ mq.ConsumerResult) error {
	if h == nil || h.service == nil || !h.service.cfg.Realtime.Enabled {
		return nil
	}
	if envelope.EventType == "order.paid" {
		var payload map[string]any
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return err
		}
		orderID, err := idFromPayload(payload, "order_id")
		if err != nil {
			return err
		}
		event, accountIDs, err := h.service.MerchantPaidOrderEvent(ctx, envelope.EventID, orderID, envelope.OccurredAt)
		if err != nil {
			return err
		}
		return h.service.PublishMerchantPaidOrder(ctx, event, accountIDs)
	}
	var rows []Delivery
	if err := h.service.db.WithContext(ctx).Where("source_event_id=?", envelope.EventID).Order("id").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		h.service.metrics.incPair(h.service.metrics.materialized, row.ClientEventType, "created")
	}
	live := rows[:0]
	for _, row := range rows {
		if row.ExpiresAt.After(time.Now()) {
			live = append(live, row)
		}
	}
	return h.service.PublishWakeups(ctx, live)
}

// RequiresSuccessfulAfterCommit makes the MQ receipt itself the retryable
// handoff for transient merchant Redis fanout. Rider events already have a
// durable realtime_deliveries relay and therefore keep the existing behavior.
func (h *MQHandler) RequiresSuccessfulAfterCommit(envelope mq.EventEnvelope, result mq.ConsumerResult) bool {
	return h != nil && h.service != nil && h.service.cfg.Realtime.Enabled && envelope.EventType == "order.paid" && result.RefType == "merchant_paid_order"
}

// targets 返回targets。
func (h *MQHandler) targets(ctx context.Context, tx *gorm.DB, envelope mq.EventEnvelope, payload map[string]any) ([]realtimeTarget, error) {
	now := time.Now().UTC()
	deliveryID := optionalIDFromPayload(payload, "delivery_order_id")
	jobID := optionalIDFromPayload(payload, "dispatch_job_id")
	base := make(map[string]any)
	copyID := func(key string) {
		if id := optionalIDFromPayload(payload, key); id != 0 {
			base[key] = strconv.FormatUint(id, 10)
		}
	}
	for _, key := range []string{"delivery_order_id", "dispatch_job_id", "offer_id", "shop_id"} {
		copyID(key)
	}

	switch envelope.EventType {
	case "dispatch.offer.created":
		riderID, err := idFromPayload(payload, "rider_id")
		if err != nil {
			return nil, err
		}
		expiresAt, err := payloadTime(payload, "expires_at")
		if err != nil {
			return nil, err
		}
		sound := stringValue(payload["sound_key"])
		if sound == "" {
			sound = "new_delivery_offer"
		}
		return []realtimeTarget{{riderID: riderID, clientEvent: "dispatch.offer.opened", aggregateType: "dispatch_offer", aggregateID: optionalIDFromPayload(payload, "offer_id"), payload: base, soundKey: sound, expiresAt: expiresAt}}, nil
	case "dispatch.offer.rejected", "dispatch.offer.expired":
		riderID, err := idFromPayload(payload, "rider_id")
		if err != nil {
			return nil, err
		}
		base["reason_code"] = closeReason(envelope.EventType, payload)
		return []realtimeTarget{{riderID: riderID, clientEvent: "dispatch.offer.closed", aggregateType: "dispatch_offer", aggregateID: optionalIDFromPayload(payload, "offer_id"), payload: base, expiresAt: now.Add(24 * time.Hour)}}, nil
	case "dispatch.grab.opened":
		if jobID == 0 {
			return nil, fmt.Errorf("dispatch_job_id is missing")
		}
		expiresAt, err := payloadTime(payload, "expires_at")
		if err != nil {
			return nil, err
		}
		riders, err := eligibleCandidateRiders(ctx, tx, jobID)
		if err != nil {
			return nil, err
		}
		return targetsForRiders(riders, "dispatch.grab.opened", "dispatch_job", jobID, base, "new_delivery_grab", expiresAt), nil
	case "dispatch.manual_required":
		if jobID == 0 {
			return nil, fmt.Errorf("dispatch_job_id is missing")
		}
		base["reason_code"] = "manual_required"
		riders, err := eligibleCandidateRiders(ctx, tx, jobID)
		if err != nil {
			return nil, err
		}
		return targetsForRiders(riders, "dispatch.grab.closed", "dispatch_job", jobID, base, "", now.Add(24*time.Hour)), nil
	case "delivery.assigned", "delivery.reassigned":
		winner, err := idFromPayload(payload, "rider_id")
		if err != nil {
			return nil, err
		}
		if deliveryID == 0 {
			return nil, fmt.Errorf("delivery_order_id is missing")
		}
		assignedPayload := cloneMap(base)
		targets := []realtimeTarget{{riderID: winner, clientEvent: "delivery.assigned", aggregateType: "delivery_order", aggregateID: deliveryID, payload: assignedPayload, expiresAt: now.Add(24 * time.Hour)}}
		if jobID == 0 {
			resolvedJobID, lookupErr := currentJobID(ctx, tx, deliveryID)
			if lookupErr != nil && !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
				return nil, lookupErr
			}
			jobID = resolvedJobID
		}
		if jobID != 0 {
			closePayload := cloneMap(base)
			closePayload["reason_code"] = "assigned_elsewhere"
			closed, err := closeJobTargets(ctx, tx, jobID, closePayload, now, winner)
			if err != nil {
				return nil, err
			}
			targets = append(targets, closed...)
		}
		if previous := optionalIDFromPayload(payload, "from_rider_id"); previous != 0 && previous != winner {
			found := false
			for i := range targets {
				if targets[i].riderID == previous && targets[i].clientEvent != "delivery.assigned" {
					targets[i].payload["reason_code"] = "reassigned"
					found = true
				}
			}
			if !found {
				closed := cloneMap(base)
				closed["reason_code"] = "reassigned"
				targets = append(targets, realtimeTarget{riderID: previous, clientEvent: "dispatch.grab.closed", aggregateType: "delivery_order", aggregateID: deliveryID, payload: closed, expiresAt: now.Add(24 * time.Hour)})
			}
		}
		return targets, nil
	case "order.cancelled":
		orderID, err := idFromPayload(payload, "order_id")
		if err != nil {
			return nil, err
		}
		base["reason_code"] = "order_cancelled"
		return cancelledOrderTargets(ctx, tx, orderID, base, now)
	default:
		return nil, mq.TerminalConsumerError("REALTIME_EVENT_UNSUPPORTED", "event is unsupported by realtime consumer", nil)
	}
}

// eligibleCandidateRiders 返回eligible Candidate Riders。
func eligibleCandidateRiders(ctx context.Context, tx *gorm.DB, jobID uint64) ([]uint64, error) {
	var riders []uint64
	err := tx.WithContext(ctx).Table("dispatch_candidates").Where("job_id=? AND eligible=1", jobID).Order("rank_no").Pluck("rider_id", &riders).Error
	return riders, err
}

// offerRiders 返回offer Riders。
func offerRiders(ctx context.Context, tx *gorm.DB, jobID uint64) ([]uint64, error) {
	var riders []uint64
	err := tx.WithContext(ctx).Table("dispatch_offers").Where("job_id=?", jobID).Order("id").Pluck("rider_id", &riders).Error
	return riders, err
}

// closeJobTargets 关闭任务 Targets并释放相关资源。
func closeJobTargets(ctx context.Context, tx *gorm.DB, jobID uint64, payload map[string]any, now time.Time, excludedRiders ...uint64) ([]realtimeTarget, error) {
	candidates, err := eligibleCandidateRiders(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	offers, err := offerRiders(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	excluded := make(map[uint64]bool, len(excludedRiders))
	for _, riderID := range excludedRiders {
		excluded[riderID] = true
	}
	targets := make([]realtimeTarget, 0, len(candidates)+len(offers))
	for _, riderID := range candidates {
		if !excluded[riderID] {
			targets = append(targets, realtimeTarget{riderID: riderID, clientEvent: "dispatch.grab.closed", aggregateType: "dispatch_job", aggregateID: jobID, payload: cloneMap(payload), expiresAt: now.Add(24 * time.Hour)})
		}
	}
	for _, riderID := range offers {
		if !excluded[riderID] {
			targets = append(targets, realtimeTarget{riderID: riderID, clientEvent: "dispatch.offer.closed", aggregateType: "dispatch_job", aggregateID: jobID, payload: cloneMap(payload), expiresAt: now.Add(24 * time.Hour)})
		}
	}
	return targets, nil
}

// cancelledOrderTargets 返回cancelled 订单 Targets。
func cancelledOrderTargets(ctx context.Context, tx *gorm.DB, orderID uint64, payload map[string]any, now time.Time) ([]realtimeTarget, error) {
	type deliveryRow struct {
		ID                   uint64
		RiderID              *uint64
		CurrentDispatchJobID *uint64
	}
	var delivery deliveryRow
	err := tx.WithContext(ctx).Table("delivery_orders").Select("id,rider_id,current_dispatch_job_id").Where("order_id=?", orderID).Order("id DESC").First(&delivery).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	payload = cloneMap(payload)
	payload["delivery_order_id"] = strconv.FormatUint(delivery.ID, 10)
	targets := make([]realtimeTarget, 0)
	if delivery.RiderID != nil {
		targets = append(targets, realtimeTarget{riderID: *delivery.RiderID, clientEvent: "dispatch.grab.closed", aggregateType: "delivery_order", aggregateID: delivery.ID, payload: cloneMap(payload), expiresAt: now.Add(24 * time.Hour)})
	}
	if delivery.CurrentDispatchJobID != nil {
		closed, err := closeJobTargets(ctx, tx, *delivery.CurrentDispatchJobID, payload, now)
		if err != nil {
			return nil, err
		}
		targets = append(targets, closed...)
	}
	return targets, nil
}

// currentJobID 返回current 任务ID。
func currentJobID(ctx context.Context, tx *gorm.DB, deliveryID uint64) (uint64, error) {
	type row struct{ CurrentDispatchJobID *uint64 }
	var current row
	err := tx.WithContext(ctx).Table("delivery_orders").Select("current_dispatch_job_id").Where("id=?", deliveryID).Take(&current).Error
	if err != nil || current.CurrentDispatchJobID == nil {
		return 0, err
	}
	return *current.CurrentDispatchJobID, nil
}

// targetsForRiders 返回targets For Riders。
func targetsForRiders(riders []uint64, eventType, aggregateType string, aggregateID uint64, payload map[string]any, sound string, expiresAt time.Time) []realtimeTarget {
	targets := make([]realtimeTarget, 0, len(riders))
	for _, riderID := range riders {
		targets = append(targets, realtimeTarget{riderID: riderID, clientEvent: eventType, aggregateType: aggregateType, aggregateID: aggregateID, payload: cloneMap(payload), soundKey: sound, expiresAt: expiresAt})
	}
	return targets
}

// payloadTime 返回载荷时间。
func payloadTime(payload map[string]any, key string) (time.Time, error) {
	value := stringValue(payload[key])
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s is invalid: %w", key, err)
	}
	return parsed.UTC(), nil
}

// closeReason 关闭Reason并释放相关资源。
func closeReason(eventType string, payload map[string]any) string {
	switch eventType {
	case "dispatch.offer.expired":
		return "expired"
	case "dispatch.offer.rejected":
		return "rejected"
	case "dispatch.manual_required":
		return "manual_required"
	default:
		if value := stringValue(payload["reason_code"]); value != "" {
			return value
		}
		return "closed"
	}
}

// stringValue 安全读取字符串指针的值。
func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

// cloneMap 克隆Map。
func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
