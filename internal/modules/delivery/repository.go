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
	// The correlated EXISTS keeps the disclosure check in the same SQL statement as
	// pagination, so an inactive/unapproved rider cannot enumerate pending work.
	db := r.db.WithContext(ctx).Model(&DeliveryOrder{}).
		Select(`delivery_orders.*,
			shops.name AS shop_name,
			COALESCE(JSON_UNQUOTE(JSON_EXTRACT(delivery_orders.recipient_snapshot,'$.district')),'') AS destination_district,
			(SELECT COALESCE(SUM(oi.quantity),0) FROM order_items oi WHERE oi.order_id=delivery_orders.order_id AND oi.deleted_at IS NULL) AS item_count,
			CASE WHEN viewer_runtime.latitude IS NOT NULL AND viewer_runtime.longitude IS NOT NULL AND shops.latitude IS NOT NULL AND shops.longitude IS NOT NULL
				THEN CAST(ROUND(ST_Distance_Sphere(POINT(viewer_runtime.longitude,viewer_runtime.latitude),POINT(shops.longitude,shops.latitude))) AS UNSIGNED)
				ELSE NULL END AS pickup_distance_m,
			dispatch_job.grab_expires_at AS grab_expires_at`).
		Joins("LEFT JOIN shops ON shops.id=delivery_orders.shop_id AND shops.deleted_at IS NULL").
		Joins("LEFT JOIN dispatch_jobs dispatch_job ON dispatch_job.id=delivery_orders.current_dispatch_job_id").
		Joins("LEFT JOIN rider_runtime_states viewer_runtime ON viewer_runtime.rider_id=?", riderID).
		Where("delivery_orders.deleted_at IS NULL").
		Where(`(
			delivery_orders.status = 'pending_assign' AND delivery_orders.dispatch_status = 'grab_open' AND delivery_orders.rider_id IS NULL AND EXISTS (
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
	db, err := pagination.ApplyFilter(db, query.Filter, deliveryFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, deliveryOrderColumns, "created_at DESC,id DESC")
	if err != nil {
		return nil, err
	}
	var rows []DeliveryOrder
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
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
