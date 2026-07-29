package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Worker struct {
	cfg                         config.CP1Config
	db                          *gorm.DB
	ids                         *snowflake.Generator
	provider                    Provider
	owner                       string
	log                         *slog.Logger
	incidentNotifications       bool
	deliveryReturnNotifications bool
}

// NewWorker 创建并初始化工作器。
func NewWorker(cfg config.CP1Config, db *gorm.DB, ids *snowflake.Generator, p Provider, owner string, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, db: db, ids: ids, provider: p, owner: owner, log: log}
}

// WithDeliveryIncidentNotifications 启用专用的商户和管理端路径。
// 异常事件绝不使用客户收件箱或客户模板降级方案。
func (w *Worker) WithDeliveryIncidentNotifications(enabled bool) *Worker {
	w.incidentNotifications = enabled
	return w
}

func (w *Worker) WithDeliveryReturnNotifications(enabled bool) *Worker {
	w.deliveryReturnNotifications = enabled
	return w
}

// Run 运行当前实例的核心处理流程。
func (w *Worker) Run(ctx context.Context) {
	w.RunWithFallback(ctx, true)
}

// RunWithFallback 运行With 降级处理流程。
func (w *Worker) RunWithFallback(ctx context.Context, fallbackEnabled bool) {
	ticker := time.NewTicker(w.cfg.WorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if fallbackEnabled {
				w.fanout(ctx)
			}
			w.deliver(ctx)
		}
	}
}

// RunOnce 运行Once处理流程。
// RunOnce 物化并投递一个有界批次，也是冒烟测试和运维恢复工具使用的安全原语。
func (w *Worker) RunOnce(ctx context.Context) {
	w.fanout(ctx)
	w.deliver(ctx)
}

type outboxRow struct {
	EventID       string
	EventType     string
	AggregateType string
	AggregateID   uint64
	Payload       datatypes.JSON
	CreatedAt     time.Time
}

var eventNames = map[string][2]string{
	"order.paid":                       {"支付成功", "您的订单已支付成功"},
	"payment.succeeded":                {"支付成功", "您的订单已支付成功"},
	"store.order.accepted":             {"门店已接单", "门店正在处理您的订单"},
	"store.order.prepared":             {"备货完成", "订单已备好，等待骑手取货"},
	"delivery.assigned":                {"骑手已接单", "订单已分配配送骑手"},
	"delivery.reassigned":              {"配送已改派", "订单已重新分配配送骑手"},
	"delivery.picked_up":               {"骑手已取货", "您的订单已由骑手取货"},
	"delivery.started":                 {"配送中", "您的订单正在配送"},
	"delivery.completed":               {"订单已送达", "感谢您的购买"},
	"delivery.force_completed":         {"订单已送达", "订单已由平台确认送达"},
	"delivery.incident.reported":       {"配送异常待处理", "有新的配送异常需要核实"},
	"delivery.incident.evidence_added": {"配送异常已补证", "骑手已补充配送异常证据"},
	"delivery.incident.acknowledged":   {"配送异常已确认", "运营已接手配送异常"},
	"delivery.incident.resolved":       {"配送异常已解决", "配送异常已完成处置"},
	"delivery.incident.rejected":       {"配送异常已驳回", "配送异常已完成核实"},
	"dispatch.offer.created":           {"新配送邀约", "您有一个新的配送订单邀约"},
	"dispatch.grab.opened":             {"新订单可抢", "有新的配送订单进入抢单池"},
	"dispatch.manual_required":         {"订单待人工派单", "订单自动调度未完成，请人工处理"},
	"order.cancelled":                  {"订单已取消", "订单已取消"},
	"refund.succeeded":                 {"退款成功", "退款已原路退回"},
	"delivery.return_requested":        {"配送退回待审核", "有新的配送退回申请需要处理"},
	"delivery.return_approved":         {"配送退回已批准", "订单退款处理中，商品正在退回门店"},
	"delivery.return_arrived":          {"退回商品已到店", "请核对交接码并验收退回商品"},
	"delivery.return_received":         {"退回商品已签收", "门店已完成退回商品验收"},
	"delivery.return_closed":           {"配送退回已完成", "退款和商品退回均已完成"},
	"delivery.return_disputed":         {"配送退回待复核", "配送退回存在冲突，需要人工复核"},
	"delivery.return_exception":        {"配送退回异常", "配送退回需要人工处理"},
	"delivery.return_sla_reminder":     {"配送退回即将逾期", "配送退回尚未完成门店签收"},
	"delivery.return_sla_breached":     {"配送退回已逾期", "配送退回超过签收时限，请立即处理"},
	"wine_ticket.purchase_issued":      {"酒票已入柜", "您购买的酒票已存入私人酒柜"},
	"wine_ticket.renewed":              {"酒票续期成功", "酒票有效期已更新，请前往私人酒柜查看"},
	"wine_ticket.refund_succeeded":     {"酒票退款成功", "退款已原路退回，请前往退款记录查看"},
	"wine_ticket.gift_claimed":         {"酒票赠礼已领取", "酒票赠礼已完成领取"},
	"wine_ticket.gift_returned":        {"酒票赠礼已退回", "未领取的酒票已退回私人酒柜"},
	"account.activation.requested":     {"账号已开通", "您的账号已完成开通"},
	"account.password_reset.requested": {"密码重置", "您的账号密码已重置"},
}

var canonicalTemplateEvent = map[string]string{"order.paid": "payment.succeeded"}

// fanout 执行通知扇出。
func (w *Worker) fanout(ctx context.Context) {
	events := make([]string, 0, len(eventNames))
	for e := range eventNames {
		events = append(events, e)
	}
	var rows []outboxRow
	// 应用 LIMIT 前排除已物化到收件箱的事件，
	// 否则旧的满批次会一直被选中，导致新事件饥饿。
	if e := w.db.WithContext(ctx).Table("outbox_events AS o").
		Select("o.event_id,o.event_type,o.aggregate_type,o.aggregate_id,o.payload,o.created_at").
		Joins("LEFT JOIN message_inboxes AS m ON m.source_event_id = o.event_id").
		Joins("LEFT JOIN mq_consumer_receipts AS r ON r.consumer_name = 'notification' AND r.event_id = o.event_id AND r.status = 'succeeded'").
		Where("o.event_type IN ? AND m.id IS NULL AND r.id IS NULL", events).
		Order("o.id").Limit(w.cfg.WorkerBatchSize).Scan(&rows).Error; e != nil {
		w.log.Error("scan notification events", slog.Any("error", e))
		return
	}
	for _, row := range rows {
		if e := w.createForEvent(ctx, row); e != nil {
			w.log.Error("fanout notification", slog.String("event_id", row.EventID), slog.Any("error", e))
		}
	}
}

// createForEvent 为事件创建通知记录。
func (w *Worker) createForEvent(ctx context.Context, event outboxRow) error {
	return w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := w.materializeForEvent(ctx, tx, event); err != nil {
			return err
		}
		now := time.Now()
		// 数据库扫描器与 MQ 路径使用相同的回执标识，
		// 使回滚和降级路径能够收敛，避免永久留下“有业务结果但无回执”的对账差异。
		return tx.Table("mq_consumer_receipts").Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "consumer_name"}, {Name: "event_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"status": "succeeded", "processed_at": now, "locked_by": nil,
				"locked_until": nil, "last_error_code": nil,
			}),
		}).Create(map[string]any{
			"id": w.ids.Next(), "consumer_name": "notification", "event_id": event.EventID,
			"event_type": event.EventType, "event_version": 1, "status": "succeeded",
			"attempts": 1, "first_received_at": now, "processed_at": now,
		}).Error
	})
}

// materializeForEvent 为事件执行通知物化。
func (w *Worker) materializeForEvent(ctx context.Context, tx *gorm.DB, event outboxRow) error {
	if strings.HasPrefix(event.EventType, "delivery.return_") {
		return w.materializeDeliveryReturnEvent(tx, event)
	}
	if strings.HasPrefix(event.EventType, "delivery.incident.") {
		return w.materializeIncidentEvent(tx, event)
	}
	if strings.HasPrefix(event.EventType, "wine_ticket.") {
		return w.materializeWineTicketEvent(tx, event)
	}
	if event.AggregateType == "account" {
		return w.materializeAccountEvent(tx, event)
	}
	orderID := event.AggregateID
	var payload map[string]any
	_ = json.Unmarshal(event.Payload, &payload)
	if id := payloadID(payload, "order_id"); id != 0 {
		orderID = id
	}
	if deliveryID := payloadID(payload, "delivery_order_id"); deliveryID != 0 {
		if e := tx.Table("delivery_orders").Select("order_id").Where("id=?", deliveryID).Scan(&orderID).Error; e != nil {
			return e
		}
	}
	if event.AggregateType == "delivery_order" {
		if e := tx.Table("delivery_orders").Select("order_id").Where("id=?", event.AggregateID).Scan(&orderID).Error; e != nil {
			return e
		}
	}
	var order struct {
		CustomerID uint64
		MerchantID uint64
	}
	if e := tx.Table("orders").Select("customer_id,merchant_id").Where("id=?", orderID).Scan(&order).Error; e != nil {
		return e
	}
	if order.CustomerID == 0 {
		return nil
	}
	text := eventNames[event.EventType]
	targetType := "order"
	if !strings.HasPrefix(event.EventType, "dispatch.") {
		message := Message{ID: w.ids.Next(), CustomerID: order.CustomerID, SourceEventID: event.EventID, Type: event.EventType, Title: text[0], Summary: text[1], TargetType: &targetType, TargetID: &orderID}
		if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&message).Error; e != nil {
			return e
		}
	}
	if !w.cfg.NotificationEnabled {
		return nil
	}
	templateEvent := event.EventType
	if canonical := canonicalTemplateEvent[event.EventType]; canonical != "" {
		templateEvent = canonical
	}
	var template Template
	e := tx.Where("event_type=? AND channel='wechat' AND status='published'", templateEvent).Order("id DESC").First(&template).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return nil
	}
	if e != nil {
		return e
	}
	recipients := []recipient{{kind: "customer", id: order.CustomerID}}
	if strings.HasPrefix(event.EventType, "dispatch.") {
		recipients = nil
	}
	if event.EventType == "dispatch.offer.created" {
		recipients = append(recipients, recipient{kind: "rider", id: payloadID(payload, "rider_id")})
	}
	if event.EventType == "dispatch.grab.opened" {
		jobID := payloadID(payload, "dispatch_job_id")
		var riderIDs []uint64
		_ = tx.Table("dispatch_candidates").Select("rider_id").Where("job_id=? AND eligible=1", jobID).Order("rank_no").Limit(500).Scan(&riderIDs).Error
		for _, riderID := range riderIDs {
			recipients = append(recipients, recipient{kind: "rider", id: riderID})
		}
	}
	if event.EventType == "dispatch.manual_required" {
		recipients = append(recipients, recipient{kind: "merchant", id: order.MerchantID})
	}
	if event.EventType == "order.paid" || event.EventType == "payment.succeeded" || event.EventType == "order.cancelled" || event.EventType == "refund.succeeded" {
		recipients = append(recipients, recipient{kind: "merchant", id: order.MerchantID})
	}
	if event.EventType == "store.order.prepared" || event.EventType == "delivery.assigned" || event.EventType == "delivery.reassigned" {
		var riderID uint64
		_ = tx.Table("delivery_orders").Select("rider_id").Where("order_id=?", orderID).Scan(&riderID).Error
		if riderID != 0 {
			recipients = append(recipients, recipient{kind: "rider", id: riderID})
		}
		if old := payloadID(payload, "from_rider_id"); old != 0 && old != riderID {
			recipients = append(recipients, recipient{kind: "rider", id: old})
		}
	}
	for _, recipient := range recipients {
		if recipient.id == 0 {
			continue
		}
		id := w.ids.Next()
		delivery := Delivery{ID: id, DeliveryNo: fmt.Sprintf("ND%d", id), EventID: event.EventID, EventType: event.EventType, RecipientType: recipient.kind, RecipientID: recipient.id, Channel: "wechat", TemplateID: template.ID, TemplateVersion: template.Version, PayloadSnapshot: event.Payload, Status: "pending"}
		if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery).Error; e != nil {
			return e
		}
	}
	return nil
}

func (w *Worker) materializeWineTicketEvent(tx *gorm.DB, event outboxRow) error {
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	recipients := []uint64{}
	switch event.EventType {
	case "wine_ticket.purchase_issued":
		recipients = append(recipients, payloadID(payload, "customer_id"))
	case "wine_ticket.renewed":
		customerID, valid, err := validateWineTicketRenewedSource(tx, event, payload)
		if err != nil {
			return err
		}
		if !valid {
			return nil
		}
		recipients = append(recipients, customerID)
	case "wine_ticket.refund_succeeded":
		customerID, valid, err := validateWineTicketRefundSource(tx, event, payload)
		if err != nil {
			return err
		}
		if !valid {
			return nil
		}
		recipients = append(recipients, customerID)
	case "wine_ticket.gift_claimed":
		recipients = append(
			recipients,
			payloadID(payload, "giver_customer_id"),
			payloadID(payload, "receiver_customer_id"),
		)
	case "wine_ticket.gift_returned":
		recipients = append(recipients, payloadID(payload, "giver_customer_id"))
	default:
		return nil
	}
	text := eventNames[event.EventType]
	targetType := event.AggregateType
	seen := make(map[uint64]struct{}, len(recipients))
	for _, customerID := range recipients {
		if customerID == 0 {
			continue
		}
		if _, exists := seen[customerID]; exists {
			continue
		}
		seen[customerID] = struct{}{}
		message := Message{
			ID: w.ids.Next(), CustomerID: customerID, SourceEventID: event.EventID,
			Type: event.EventType, Title: text[0], Summary: text[1],
			TargetType: &targetType, TargetID: &event.AggregateID,
			CreatedAt: event.CreatedAt,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&message).Error; err != nil {
			return err
		}
	}
	return nil
}

func validateWineTicketRenewedSource(
	tx *gorm.DB,
	event outboxRow,
	payload map[string]any,
) (uint64, bool, error) {
	if event.AggregateType != "wine_ticket_renewal" {
		return 0, false, nil
	}
	customerID := payloadID(payload, "customer_id")
	renewalNo, _ := payload["renewal_no"].(string)
	if customerID == 0 || renewalNo == "" {
		return 0, false, nil
	}
	var matched int64
	err := tx.Table("wine_ticket_renewals").
		Where(
			"id=? AND customer_id=? AND renewal_no=? AND status='completed'",
			event.AggregateID,
			customerID,
			renewalNo,
		).
		Count(&matched).Error
	return customerID, matched == 1, err
}

func validateWineTicketRefundSource(
	tx *gorm.DB,
	event outboxRow,
	payload map[string]any,
) (uint64, bool, error) {
	if event.AggregateType != "wine_ticket_refund" {
		return 0, false, nil
	}
	customerID := payloadID(payload, "customer_id")
	refundNo, _ := payload["refund_no"].(string)
	purchaseNo, _ := payload["purchase_no"].(string)
	if customerID == 0 || refundNo == "" || purchaseNo == "" {
		return 0, false, nil
	}
	var matched int64
	err := tx.Table("wine_ticket_refunds AS refund").
		Joins("JOIN wine_ticket_purchases AS purchase ON purchase.id=refund.purchase_id").
		Where(
			"refund.id=? AND refund.customer_id=? AND refund.wine_ticket_refund_no=? AND refund.status='succeeded' AND purchase.purchase_no=?",
			event.AggregateID,
			customerID,
			refundNo,
			purchaseNo,
		).
		Count(&matched).Error
	return customerID, matched == 1, err
}

func (w *Worker) materializeDeliveryReturnEvent(tx *gorm.DB, event outboxRow) error {
	if !w.deliveryReturnNotifications {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	orderID := payloadID(payload, "order_id")
	shopID := payloadID(payload, "shop_id")
	riderID := payloadID(payload, "rider_id")
	returnID := payloadID(payload, "delivery_return_id")
	var order struct {
		CustomerID uint64
		MerchantID uint64
	}
	if orderID != 0 {
		if err := tx.Table("orders").Select("customer_id,merchant_id").Where("id=?", orderID).Scan(&order).Error; err != nil {
			return err
		}
	}
	if order.MerchantID == 0 && shopID != 0 {
		if err := tx.Table("shops").Select("merchant_id").Where("id=? AND deleted_at IS NULL", shopID).Scan(&order.MerchantID).Error; err != nil {
			return err
		}
	}
	var adminIDs []uint64
	if err := tx.Table("admin_users au").Distinct("au.id").
		Joins("JOIN role_permissions rp ON rp.role_id=au.role_id AND rp.deleted_at IS NULL").
		Joins("JOIN permissions p ON p.id=rp.permission_id AND p.code='delivery_return:list_all' AND p.status='active' AND p.deleted_at IS NULL").
		Where("au.status='active' AND au.deleted_at IS NULL").Scan(&adminIDs).Error; err != nil {
		return err
	}
	recipients := make([]recipient, 0, len(adminIDs)+3)
	addAdmins := func() {
		for _, adminID := range adminIDs {
			recipients = append(recipients, recipient{kind: "admin", id: adminID})
		}
	}
	switch event.EventType {
	case "delivery.return_requested":
		recipients = append(recipients, recipient{kind: "merchant", id: order.MerchantID})
		addAdmins()
	case "delivery.return_approved":
		recipients = append(recipients, recipient{kind: "customer", id: order.CustomerID}, recipient{kind: "rider", id: riderID}, recipient{kind: "merchant", id: order.MerchantID})
	case "delivery.return_arrived":
		recipients = append(recipients, recipient{kind: "merchant", id: order.MerchantID})
	case "delivery.return_received", "delivery.return_closed":
		recipients = append(recipients, recipient{kind: "customer", id: order.CustomerID})
		addAdmins()
	default:
		recipients = append(recipients, recipient{kind: "merchant", id: order.MerchantID})
		addAdmins()
	}
	text := eventNames[event.EventType]
	targetType := "delivery_return"
	for _, target := range recipients {
		if target.kind != "customer" || target.id == 0 {
			continue
		}
		message := Message{ID: w.ids.Next(), CustomerID: target.id, SourceEventID: event.EventID, Type: event.EventType,
			Title: text[0], Summary: text[1], TargetType: &targetType, TargetID: &returnID}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&message).Error; err != nil {
			return err
		}
	}
	if !w.cfg.NotificationEnabled {
		return nil
	}
	var template Template
	if err := tx.Where("event_type=? AND channel='wechat' AND status='published'", event.EventType).Order("id DESC").First(&template).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		w.log.Warn("delivery return notification route is not configured", slog.String("event_id", event.EventID), slog.String("event_type", event.EventType))
		return nil
	} else if err != nil {
		return err
	}
	for _, target := range recipients {
		if target.id == 0 {
			continue
		}
		id := w.ids.Next()
		delivery := Delivery{ID: id, DeliveryNo: fmt.Sprintf("ND%d", id), EventID: event.EventID, EventType: event.EventType,
			RecipientType: target.kind, RecipientID: target.id, Channel: "wechat", TemplateID: template.ID,
			TemplateVersion: template.Version, PayloadSnapshot: event.Payload, Status: "pending"}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery).Error; err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) materializeIncidentEvent(tx *gorm.DB, event outboxRow) error {
	if !w.incidentNotifications {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	shopID := payloadID(payload, "shop_id")
	if shopID == 0 {
		return nil
	}
	var merchantID uint64
	if err := tx.Table("shops").Select("merchant_id").Where("id=? AND deleted_at IS NULL", shopID).Scan(&merchantID).Error; err != nil {
		return err
	}
	var adminIDs []uint64
	if err := tx.Table("admin_users au").Distinct("au.id").
		Joins("JOIN role_permissions rp ON rp.role_id=au.role_id AND rp.deleted_at IS NULL").
		Joins("JOIN permissions p ON p.id=rp.permission_id AND p.code='delivery_incident:list_all' AND p.status='active' AND p.deleted_at IS NULL").
		Where("au.status='active' AND au.deleted_at IS NULL").Scan(&adminIDs).Error; err != nil {
		return err
	}
	var template Template
	if err := tx.Where("event_type=? AND channel='wechat' AND status='published'", event.EventType).Order("id DESC").First(&template).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		w.log.Warn("delivery incident notification route is not configured", slog.String("event_id", event.EventID), slog.String("event_type", event.EventType))
		return nil
	} else if err != nil {
		return err
	}
	recipients := []recipient{}
	if merchantID != 0 {
		recipients = append(recipients, recipient{kind: "merchant", id: merchantID})
	}
	for _, adminID := range adminIDs {
		recipients = append(recipients, recipient{kind: "admin", id: adminID})
	}
	for _, target := range recipients {
		id := w.ids.Next()
		delivery := Delivery{ID: id, DeliveryNo: fmt.Sprintf("ND%d", id), EventID: event.EventID, EventType: event.EventType,
			RecipientType: target.kind, RecipientID: target.id, Channel: "wechat", TemplateID: template.ID,
			TemplateVersion: template.Version, PayloadSnapshot: event.Payload, Status: "pending"}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery).Error; err != nil {
			return err
		}
	}
	return nil
}

// materializeAccountEvent 物化账户事件。
func (w *Worker) materializeAccountEvent(tx *gorm.DB, event outboxRow) error {
	if !w.cfg.NotificationEnabled {
		return nil
	}
	var template Template
	err := tx.Where("event_type=? AND channel='wechat' AND status='published'", event.EventType).Order("id DESC").First(&template).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	id := w.ids.Next()
	delivery := Delivery{ID: id, DeliveryNo: fmt.Sprintf("ND%d", id), EventID: event.EventID, EventType: event.EventType, RecipientType: "account", RecipientID: event.AggregateID, Channel: "wechat", TemplateID: template.ID, TemplateVersion: template.Version, PayloadSnapshot: event.Payload, Status: "pending"}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery).Error
}

// MaterializeEvent 返回Materialize 事件。
// MaterializeEvent 供 MQ 运行时使用。事务由调用方持有，
// 因此通知副作用和消费者回执会原子提交。
func (w *Worker) MaterializeEvent(ctx context.Context, tx *gorm.DB, eventID, eventType, aggregateType string, aggregateID uint64, payload datatypes.JSON, occurredAt time.Time) error {
	if _, ok := eventNames[eventType]; !ok {
		return fmt.Errorf("notification event %s is not supported", eventType)
	}
	return w.materializeForEvent(ctx, tx, outboxRow{EventID: eventID, EventType: eventType, AggregateType: aggregateType, AggregateID: aggregateID, Payload: payload, CreatedAt: occurredAt})
}

type recipient struct {
	kind string
	id   uint64
}

// payloadID 返回载荷ID。
func payloadID(payload map[string]any, key string) uint64 {
	value := payload[key]
	switch typed := value.(type) {
	case string:
		id, _ := strconv.ParseUint(typed, 10, 64)
		return id
	case float64:
		if typed > 0 {
			return uint64(typed)
		}
	}
	return 0
}

// deliver 投递通知。
func (w *Worker) deliver(ctx context.Context) {
	for i := 0; i < w.cfg.WorkerBatchSize; i++ {
		task, ok, e := w.claim(ctx)
		if e != nil {
			w.log.Error("claim notification", slog.Any("error", e))
			return
		}
		if !ok {
			return
		}
		w.send(ctx, task)
	}
}

// claim 认领配送。
func (w *Worker) claim(ctx context.Context) (Delivery, bool, error) {
	var row Delivery
	e := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		until := now.Add(30 * time.Second)
		claimToken := w.owner + ":" + uuid.NewString()
		result := tx.Exec(`
			UPDATE notification_deliveries SET status='processing', locked_by=?, locked_until=?
			WHERE ((status IN ('pending','retry_wait') AND (next_retry_at IS NULL OR next_retry_at<=?))
			    OR (status='processing' AND locked_until<?))
			  AND (locked_until IS NULL OR locked_until<?)
			ORDER BY id LIMIT 1
		`, claimToken, until, now, now, now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		return tx.Where("status='processing' AND locked_by=?", claimToken).First(&row).Error
	})
	return row, row.ID != 0, e
}

// send 发送通知。
func (w *Worker) send(ctx context.Context, row Delivery) {
	providerID := "notify-" + row.DeliveryNo
	result, e := w.provider.Send(ctx, SendRequest{ProviderRequestID: providerID, TemplateID: fmt.Sprint(row.TemplateID), Recipient: fmt.Sprint(row.RecipientID), Payload: row.PayloadSnapshot})
	var unknown *ProviderError
	if errors.As(e, &unknown) && unknown.Unknown {
		result, e = w.provider.Query(ctx, providerID)
	}
	now := time.Now()
	status := "succeeded"
	var code *string
	var next *time.Time
	if e != nil {
		retry := true
		c := "provider_failure"
		var pe *ProviderError
		if errors.As(e, &pe) {
			retry = pe.Retryable
			c = pe.Code
		}
		code = &c
		if retry && row.Attempts+1 < 5 {
			status = "retry_wait"
			n := now.Add(time.Duration(row.Attempts+1) * 30 * time.Second)
			next = &n
		} else {
			status = "dead"
		}
	}
	if strings.TrimSpace(result.ProviderRequestID) != "" {
		providerID = result.ProviderRequestID
	}
	updates := map[string]any{"status": status, "attempts": row.Attempts + 1, "provider_request_id": providerID, "last_error_code": code, "next_retry_at": next, "locked_by": nil, "locked_until": nil}
	if status == "succeeded" {
		updates["sent_at"] = &now
	}
	if e := w.db.WithContext(ctx).Model(&Delivery{}).Where("id=? AND locked_by=?", row.ID, deliveryLeaseOwner(row, w.owner)).Updates(updates).Error; e != nil {
		w.log.Error("finish notification", slog.Any("error", e))
	}
}

// deliveryLeaseOwner 返回配送租约 Owner。
func deliveryLeaseOwner(row Delivery, fallback string) string {
	if row.LockedBy != nil && *row.LockedBy != "" {
		return *row.LockedBy
	}
	return fallback
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(v any) datatypes.JSON { raw, _ := json.Marshal(v); return raw }
