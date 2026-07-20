package deliveryincident

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }
func (r *Repository) DB() *gorm.DB          { return r.db }

func (r *Repository) LockDelivery(ctx context.Context, tx *gorm.DB, id uint64) (DeliveryOrder, error) {
	var row DeliveryOrder
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id=? AND deleted_at IS NULL", id).First(&row).Error
	return row, err
}

func (r *Repository) IncidentRef(ctx context.Context, tx *gorm.DB, id uint64) (Incident, error) {
	var row Incident
	err := tx.WithContext(ctx).Select("id,delivery_order_id").First(&row, "id=?", id).Error
	return row, err
}

func (r *Repository) LockIncident(ctx context.Context, tx *gorm.DB, id uint64) (Incident, error) {
	var row Incident
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id=?", id).Error
	return row, err
}

func (r *Repository) OrderItems(ctx context.Context, tx *gorm.DB, orderID uint64, ids []uint64) ([]OrderItemRow, error) {
	query := tx.WithContext(ctx).Where("order_id=? AND deleted_at IS NULL", orderID)
	if len(ids) > 0 {
		query = query.Where("id IN ?", ids)
	}
	var rows []OrderItemRow
	err := query.Order("id").Find(&rows).Error
	return rows, err
}

func (r *Repository) CreateIncident(ctx context.Context, tx *gorm.DB, row *Incident) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *Repository) CreateItems(ctx context.Context, tx *gorm.DB, rows []Item) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Create(&rows).Error
}

func (r *Repository) CreateEvidence(ctx context.Context, tx *gorm.DB, rows []Evidence) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Create(&rows).Error
}

func (r *Repository) CreateHistory(ctx context.Context, tx *gorm.DB, row History) error {
	return tx.WithContext(ctx).Create(&row).Error
}

func (r *Repository) CreateAudit(ctx context.Context, tx *gorm.DB, row AuditLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, row OutboxEvent) error {
	return tx.WithContext(ctx).Create(&row).Error
}

func (r *Repository) UpdateIncidentVersioned(ctx context.Context, tx *gorm.DB, id uint64, version uint, values map[string]any) (bool, error) {
	values["version"] = gorm.Expr("version+1")
	result := tx.WithContext(ctx).Model(&Incident{}).Where("id=? AND version=?", id, version).Updates(values)
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) EvidenceCount(ctx context.Context, tx *gorm.DB, incidentID uint64) (int64, error) {
	var count int64
	err := tx.WithContext(ctx).Model(&Evidence{}).Where("incident_id=?", incidentID).Count(&count).Error
	return count, err
}

func (r *Repository) ActiveByType(ctx context.Context, tx *gorm.DB, deliveryID uint64, incidentType string) (Incident, error) {
	var row Incident
	err := tx.WithContext(ctx).Where("delivery_order_id=? AND type=? AND status IN ?", deliveryID, incidentType, activeStatuses).
		Order("id DESC").First(&row).Error
	return row, err
}

func (r *Repository) ActiveForUpdate(ctx context.Context, tx *gorm.DB, deliveryID uint64, stage string) ([]Incident, error) {
	query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("delivery_order_id=? AND status IN ?", deliveryID, activeStatuses)
	if stage != "" {
		query = query.Where("stage=?", stage)
	}
	var rows []Incident
	err := query.Order("id ASC").Find(&rows).Error
	return rows, err
}

func (r *Repository) Aggregate(ctx context.Context, db *gorm.DB, incidentID uint64) (Aggregate, error) {
	var out Aggregate
	if err := db.WithContext(ctx).First(&out.Incident, "id=?", incidentID).Error; err != nil {
		return Aggregate{}, err
	}
	if err := r.loadChildren(ctx, db, &out); err != nil {
		return Aggregate{}, err
	}
	return out, nil
}

func (r *Repository) RiderAggregate(ctx context.Context, incidentID, riderID uint64) (Aggregate, error) {
	var out Aggregate
	err := r.db.WithContext(ctx).Model(&Incident{}).
		Where("delivery_incidents.id=?", incidentID).
		Where("delivery_incidents.rider_id=? OR EXISTS (SELECT 1 FROM delivery_orders d WHERE d.id=delivery_incidents.delivery_order_id AND d.rider_id=? AND d.deleted_at IS NULL)", riderID, riderID).
		First(&out.Incident).Error
	if err != nil {
		return Aggregate{}, err
	}
	if err := r.loadChildren(ctx, r.db, &out); err != nil {
		return Aggregate{}, err
	}
	return out, nil
}

func (r *Repository) StoreAggregate(ctx context.Context, incidentID uint64, shopIDs []uint64) (Aggregate, error) {
	var out Aggregate
	err := r.db.WithContext(ctx).Where("id=? AND shop_id IN ?", incidentID, shopIDs).First(&out.Incident).Error
	if err != nil {
		return Aggregate{}, err
	}
	if err := r.loadChildren(ctx, r.db, &out); err != nil {
		return Aggregate{}, err
	}
	return out, nil
}

func (r *Repository) loadChildren(ctx context.Context, db *gorm.DB, out *Aggregate) error {
	if err := db.WithContext(ctx).Where("incident_id=?", out.Incident.ID).Order("id").Find(&out.Items).Error; err != nil {
		return err
	}
	if err := db.WithContext(ctx).Where("incident_id=?", out.Incident.ID).Order("id").Find(&out.Evidence).Error; err != nil {
		return err
	}
	return db.WithContext(ctx).Where("incident_id=?", out.Incident.ID).Order("created_at,id").Find(&out.History).Error
}

func (r *Repository) RiderList(ctx context.Context, riderID, deliveryID uint64, query pagination.Query, filters ListFilters) ([]Incident, error) {
	db := r.db.WithContext(ctx).Model(&Incident{}).
		Where("delivery_incidents.delivery_order_id=?", deliveryID).
		Where("EXISTS (SELECT 1 FROM delivery_orders d WHERE d.id=delivery_incidents.delivery_order_id AND d.rider_id=? AND d.deleted_at IS NULL)", riderID)
	return r.list(db, query, filters, nil, false)
}

func (r *Repository) StoreList(ctx context.Context, shopIDs []uint64, query pagination.Query, filters ListFilters) ([]Incident, error) {
	db := r.db.WithContext(ctx).Model(&Incident{}).Where("delivery_incidents.shop_id IN ?", shopIDs)
	return r.list(db, query, filters, nil, false)
}

func (r *Repository) AdminList(ctx context.Context, query pagination.Query, filters ListFilters) ([]Incident, error) {
	db := r.db.WithContext(ctx).Model(&Incident{})
	return r.list(db, query, filters, nil, filters.Status == "")
}

func (r *Repository) list(db *gorm.DB, query pagination.Query, filters ListFilters, shopIDs []uint64, defaultActive bool) ([]Incident, error) {
	if len(shopIDs) > 0 {
		db = db.Where("delivery_incidents.shop_id IN ?", shopIDs)
	}
	if filters.Type != "" {
		db = db.Where("delivery_incidents.type=?", filters.Type)
	}
	if filters.Status != "" {
		db = db.Where("delivery_incidents.status=?", filters.Status)
	} else if defaultActive {
		db = db.Where("delivery_incidents.status IN ?", activeStatuses)
	}
	if filters.Stage != "" {
		db = db.Where("delivery_incidents.stage=?", filters.Stage)
	}
	if filters.ShopID != nil {
		db = db.Where("delivery_incidents.shop_id=?", *filters.ShopID)
	}
	if filters.RiderID != nil {
		db = db.Where("delivery_incidents.rider_id=?", *filters.RiderID)
	}
	if filters.IncidentNo != "" {
		db = db.Where("delivery_incidents.incident_no LIKE ?", filters.IncidentNo+"%")
	}
	if filters.OrderNo != "" {
		db = db.Where("EXISTS (SELECT 1 FROM orders o WHERE o.id=delivery_incidents.order_id AND o.order_no LIKE ? AND o.deleted_at IS NULL)", filters.OrderNo+"%")
	}
	if filters.ReportedFrom != nil {
		db = db.Where("delivery_incidents.reported_at>=?", *filters.ReportedFrom)
	}
	if filters.ReportedTo != nil {
		db = db.Where("delivery_incidents.reported_at<=?", *filters.ReportedTo)
	}
	var err error
	db, err = pagination.ApplyFilter(db, query.Filter, incidentFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, incidentOrderColumns, "delivery_incidents.reported_at DESC,delivery_incidents.id DESC")
	if err != nil {
		return nil, err
	}
	var rows []Incident
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

var incidentFilterColumns = map[string]string{
	"id": "delivery_incidents.id", "shop_id": "delivery_incidents.shop_id", "rider_id": "delivery_incidents.rider_id",
	"status": "delivery_incidents.status", "type": "delivery_incidents.type", "stage": "delivery_incidents.stage",
	"reported_at": "delivery_incidents.reported_at", "incident_no": "delivery_incidents.incident_no",
}

var incidentOrderColumns = map[string]string{
	"id": "delivery_incidents.id", "status": "delivery_incidents.status", "reported_at": "delivery_incidents.reported_at",
	"created_at": "delivery_incidents.created_at", "updated_at": "delivery_incidents.updated_at",
}

func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
