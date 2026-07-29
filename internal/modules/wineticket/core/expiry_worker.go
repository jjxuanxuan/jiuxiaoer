package core

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

var activeExpiryGuardRenewalStatuses = []string{
	"pending_payment",
	"payment_unknown",
	"applying",
	"compensating_refund",
	"refund_exception",
}

// ExpiryWorker 负责由时间驱动的 Lot 生命周期转换。
// 通知发送不属于本任务，因此关闭客户提醒时，权益过期仍可正常执行。
type ExpiryWorker struct {
	repo   *expiryRepository
	assets *AssetService
	now    func() time.Time
	batch  int
}

func NewExpiryWorker(db *gorm.DB, ids *snowflake.Generator) *ExpiryWorker {
	return &ExpiryWorker{
		repo:   &expiryRepository{db: db},
		assets: NewAssetService(ids),
		now:    time.Now,
		batch:  100,
	}
}

func (w *ExpiryWorker) WithNow(now func() time.Time) *ExpiryWorker {
	if now != nil {
		w.now = now
	}
	return w
}

func (w *ExpiryWorker) WithBatchSize(batch int) *ExpiryWorker {
	if batch > 0 && batch <= 1000 {
		w.batch = batch
	}
	return w
}

// ExpireLotsOnce 使用 SKIP LOCKED 锁定一批有上限的到期记录。
// 续期和退款保护仍归其来源业务所有，本任务不会释放这些保护。
func (w *ExpiryWorker) ExpireLotsOnce(ctx context.Context) (int, error) {
	now := NowShanghai(w.now)
	expired := 0
	err := w.repo.withTransaction(ctx, func(tx *gorm.DB) error {
		lots, err := w.repo.dueLots(ctx, tx, now, w.batch)
		if err != nil {
			return err
		}
		for index := range lots {
			lot := &lots[index]
			blocked, err := w.repo.expiryGuarded(ctx, tx, lot.ID)
			if err != nil {
				return err
			}
			if blocked {
				continue
			}
			created, err := w.expireAvailable(
				ctx,
				tx,
				lot,
				now,
				fmt.Sprintf("expiry:%d:%d", lot.ID, lot.ExpiresAt.UnixMilli()),
			)
			if err != nil {
				return err
			}
			if created || lot.Status == LotStatusExpired {
				expired++
			}
		}
		return nil
	})
	return expired, err
}

func (w *ExpiryWorker) expireAvailable(
	ctx context.Context,
	tx *gorm.DB,
	lot *Lot,
	now time.Time,
	actionKey string,
) (bool, error) {
	result, err := w.assets.Expire(
		ctx,
		NewTransactionAssetRepository(tx),
		ExpireCommand{
			LotID:           lot.ID,
			OwnerCustomerID: lot.OwnerCustomerID,
			TransactionType: TransactionTypeExpiry,
			BizType:         "wine_ticket_expiry",
			BizID:           lot.ID,
			ActionKey:       actionKey,
			Metadata: map[string]any{
				"reason":     "lot_expired",
				"expires_at": FormatShanghai(lot.ExpiresAt),
			},
			OccurredAt: now,
		},
	)
	if err != nil {
		return false, err
	}
	*lot = result.Lot
	return len(result.Transactions) > 0 && !result.Replayed, nil
}
