package ops

import (
	"context"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/cabinet"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
)

const (
	LotSourcePurchase = core.LotSourcePurchase

	LotStatusActive   = core.LotStatusActive
	LotStatusDepleted = core.LotStatusDepleted
	LotStatusExpired  = core.LotStatusExpired
	LotStatusRefunded = core.LotStatusRefunded
)

// PurchaseProjector 是运营购买查询使用的下层子域窄读取契约。
type PurchaseProjector interface {
	DTOsFromRecords(
		context.Context,
		[]purchase.PurchaseRecord,
	) ([]purchase.PurchaseDTO, error)
}

func purchaseProjection(db *gorm.DB) *gorm.DB {
	return purchase.PurchaseProjection(db)
}

func lotFactProjection(db *gorm.DB) *gorm.DB {
	return purchase.LotFactProjection(db)
}

func validPurchaseStatus(status string) bool {
	return purchase.ValidPurchaseStatus(status)
}

func validLotStatus(status string) bool {
	return cabinet.ValidLotStatus(status)
}

func lotRecordDTO(
	row purchase.LotFactRecord,
	now time.Time,
	transactions []cabinet.WineTicketTransactionDTO,
) (cabinet.LotDTO, error) {
	return cabinet.ProjectLot(cabinet.LotFactRecord(row), now, transactions)
}
