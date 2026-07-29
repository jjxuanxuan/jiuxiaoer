package delivery

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct {
	db *gorm.DB
}

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// DB 返回当前数据仓储使用的数据库连接。
func (r *Repository) DB() *gorm.DB {
	return r.db
}

// List 查询配送 Order列表列表。
func (r *Repository) List(ctx context.Context, riderID uint64, status string, query pagination.Query) ([]DeliveryOrder, error) {
	// 骑手只能看到服务范围内的待接任务，以及已经分配给自己的任务。
	// 关联 EXISTS 将信息披露检查与分页保持在同一 SQL 语句中，
	// 使停用或未审核骑手无法枚举待处理任务。
	db := r.db.WithContext(ctx).Model(&DeliveryOrder{}).
		Select(`delivery_orders.*,
			shops.name AS shop_name,
			COALESCE(JSON_UNQUOTE(JSON_EXTRACT(delivery_orders.recipient_snapshot,'$.district')),'') AS destination_district,
			(SELECT COALESCE(SUM(oi.quantity),0) FROM order_items oi WHERE oi.order_id=delivery_orders.order_id AND oi.deleted_at IS NULL) AS item_count,
			CASE WHEN viewer_runtime.latitude IS NOT NULL AND viewer_runtime.longitude IS NOT NULL AND shops.latitude IS NOT NULL AND shops.longitude IS NOT NULL
				THEN CAST(ROUND(ST_Distance_Sphere(POINT(viewer_runtime.longitude,viewer_runtime.latitude),POINT(shops.longitude,shops.latitude))) AS UNSIGNED)
				ELSE NULL END AS pickup_distance_m,
			dispatch_job.grab_expires_at AS grab_expires_at,
			customer_order.order_type AS order_type,
			customer_order.settlement_mode AS settlement_mode`).
		Joins("LEFT JOIN shops ON shops.id=delivery_orders.shop_id AND shops.deleted_at IS NULL").
		Joins("JOIN orders customer_order ON customer_order.id=delivery_orders.order_id AND customer_order.deleted_at IS NULL").
		Joins("LEFT JOIN dispatch_jobs dispatch_job ON dispatch_job.id=delivery_orders.current_dispatch_job_id").
		Joins("LEFT JOIN rider_runtime_states viewer_runtime ON viewer_runtime.rider_id=?", riderID).
		Where("delivery_orders.deleted_at IS NULL").
		Where(`(
			delivery_orders.status = 'pending_assign'
			AND delivery_orders.dispatch_status = 'grab_open'
			AND delivery_orders.rider_id IS NULL
			AND (delivery_orders.not_before_at IS NULL OR delivery_orders.not_before_at <= CURRENT_TIMESTAMP(3))
			AND EXISTS (
				SELECT 1
				FROM riders r
				JOIN accounts a ON a.id = r.account_id AND a.deleted_at IS NULL
				JOIN rider_service_shops rss ON rss.rider_id = r.id AND rss.shop_id = delivery_orders.shop_id AND rss.status = 'active'
				JOIN rider_runtime_states rrs ON rrs.rider_id = r.id AND rrs.work_status = 'online'
				JOIN dispatch_jobs dj ON dj.id = delivery_orders.current_dispatch_job_id
				JOIN shops eligible_shop ON eligible_shop.id = delivery_orders.shop_id AND eligible_shop.deleted_at IS NULL
				WHERE r.id = ?
					AND r.deleted_at IS NULL
					AND r.status = 'active'
					AND r.review_status = 'approved'
					AND a.status = 'active'
					AND dj.status = 'grab_open'
					AND dj.grab_expires_at > CURRENT_TIMESTAMP(3)
					AND rrs.heartbeat_at IS NOT NULL
					AND rrs.captured_at IS NOT NULL
					AND rrs.latitude IS NOT NULL
					AND rrs.longitude IS NOT NULL
					AND rrs.accuracy_m IS NOT NULL
					AND eligible_shop.latitude IS NOT NULL
					AND eligible_shop.longitude IS NOT NULL
					AND TIMESTAMPDIFF(SECOND, rrs.heartbeat_at, CURRENT_TIMESTAMP(3)) BETWEEN 0 AND COALESCE(CAST(JSON_UNQUOTE(JSON_EXTRACT(dj.policy_snapshot, '$.heartbeat_fresh_seconds')) AS UNSIGNED), 60)
					AND TIMESTAMPDIFF(SECOND, rrs.captured_at, CURRENT_TIMESTAMP(3)) BETWEEN 0 AND COALESCE(CAST(JSON_UNQUOTE(JSON_EXTRACT(dj.policy_snapshot, '$.location_fresh_seconds')) AS UNSIGNED), 120)
					AND rrs.accuracy_m <= COALESCE(CAST(JSON_UNQUOTE(JSON_EXTRACT(dj.policy_snapshot, '$.max_location_accuracy_m')) AS UNSIGNED), 200)
					AND ST_Distance_Sphere(POINT(rrs.longitude,rrs.latitude),POINT(eligible_shop.longitude,eligible_shop.latitude))
						<= COALESCE(CAST(JSON_UNQUOTE(JSON_EXTRACT(dj.policy_snapshot, '$.max_pickup_distance_m')) AS UNSIGNED), 5000)
					AND (SELECT COUNT(*) FROM delivery_orders active_delivery WHERE active_delivery.rider_id=r.id AND active_delivery.status IN ('accepted','delivering') AND active_delivery.deleted_at IS NULL)
						< COALESCE(rrs.max_active_orders, CAST(JSON_UNQUOTE(JSON_EXTRACT(dj.policy_snapshot, '$.max_active_orders_default')) AS UNSIGNED), 3)
			)
		) OR delivery_orders.rider_id = ?`, riderID, riderID)
	if status != "" {
		db = db.Where("delivery_orders.status = ?", status)
	}
	db, err := pagination.ApplyTimeIDCursor(db, query, "delivery_orders.created_at", "delivery_orders.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []DeliveryOrder
	err = pagination.OffsetDB(db, query).Order("delivery_orders.created_at DESC, delivery_orders.id DESC").Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// Detail 在一个事务中读取已分配配送及其全部不可变履约上下文。
// 关联的有效分配条件是信息披露边界：对候选骑手、已被替换骑手和无关骑手而言，
// 该配送均表现为不存在。
func (r *Repository) Detail(ctx context.Context, riderID uint64, deliveryID uint64) (DeliveryOrder, Order, Shop, []OrderItem, error) {
	var deliveryRow DeliveryOrder
	var orderRow Order
	var shopRow Shop
	items := make([]OrderItem, 0)

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&DeliveryOrder{}).
			Where("delivery_orders.id = ? AND delivery_orders.rider_id = ? AND delivery_orders.deleted_at IS NULL", deliveryID, riderID).
			Where(`EXISTS (
				SELECT 1 FROM delivery_assignments current_assignment
				WHERE current_assignment.delivery_order_id = delivery_orders.id
					AND current_assignment.to_rider_id = ?
					AND current_assignment.status = 'active'
			)`, riderID).
			First(&deliveryRow).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ? AND deleted_at IS NULL", deliveryRow.OrderID).First(&orderRow).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ? AND deleted_at IS NULL", deliveryRow.ShopID).First(&shopRow).Error; err != nil {
			return err
		}
		return tx.Where("order_id = ? AND deleted_at IS NULL", deliveryRow.OrderID).
			Order("id ASC").Find(&items).Error
	})
	return deliveryRow, orderRow, shopRow, items, err
}

// AssignedSummary 重新加载分配后的记录，使接单响应携带与已分配列表项
// 相同的强类型履约快照。
func (r *Repository) AssignedSummary(ctx context.Context, riderID, deliveryID uint64) (DeliveryOrder, error) {
	var row DeliveryOrder
	err := r.db.WithContext(ctx).Model(&DeliveryOrder{}).
		Select(`
			delivery_orders.*,
			customer_order.order_type AS order_type,
			customer_order.settlement_mode AS settlement_mode
		`).
		Joins(`
			JOIN orders customer_order
			  ON customer_order.id = delivery_orders.order_id
			 AND customer_order.deleted_at IS NULL
		`).
		Where(
			"delivery_orders.id=? AND delivery_orders.rider_id=? AND delivery_orders.deleted_at IS NULL",
			deliveryID,
			riderID,
		).
		Where(`EXISTS (
			SELECT 1 FROM delivery_assignments current_assignment
			WHERE current_assignment.delivery_order_id=delivery_orders.id
				AND current_assignment.to_rider_id=?
				AND current_assignment.status='active'
		)`, riderID).
		First(&row).Error
	return row, err
}

// LockDelivery 串行化骑手接单和配送状态流转。
func (r *Repository) LockDelivery(ctx context.Context, tx *gorm.DB, deliveryID uint64) (DeliveryOrder, error) {
	var row DeliveryOrder
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", deliveryID).
		First(&row).Error
	return row, err
}

// LockOrder 保证 orders.status 和 delivery_orders.status 在同一事务内同步。
func (r *Repository) LockOrder(ctx context.Context, tx *gorm.DB, orderID uint64) (Order, error) {
	var row Order
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", orderID).
		First(&row).Error
	return row, err
}

// UpdateDelivery 更新配送。
func (r *Repository) UpdateDelivery(ctx context.Context, tx *gorm.DB, deliveryID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&DeliveryOrder{}).Where("id = ?", deliveryID).Updates(values).Error
}

// UpdateOrder 更新订单。
func (r *Repository) UpdateOrder(ctx context.Context, tx *gorm.DB, orderID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&Order{}).Where("id = ?", orderID).Updates(values).Error
}

// CreateOrderLog 创建订单日志。
func (r *Repository) CreateOrderLog(ctx context.Context, tx *gorm.DB, row OrderLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateAuditLog 创建审计日志。
func (r *Repository) CreateAuditLog(ctx context.Context, tx *gorm.DB, row AuditLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateOutbox 创建发件箱事件。
func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, row OutboxEvent) error {
	return tx.WithContext(ctx).Create(&row).Error
}

var deliveryOrderColumns = map[string]string{
	"id":              "delivery_orders.id",
	"created_at":      "delivery_orders.created_at",
	"updated_at":      "delivery_orders.updated_at",
	"status":          "delivery_orders.status",
	"shop_id":         "delivery_orders.shop_id",
	"dispatch_status": "delivery_orders.dispatch_status",
}

var deliveryFilterColumns = map[string]string{
	"id":              "delivery_orders.id",
	"shop_id":         "delivery_orders.shop_id",
	"status":          "delivery_orders.status",
	"created_at":      "delivery_orders.created_at",
	"dispatch_status": "delivery_orders.dispatch_status",
}
