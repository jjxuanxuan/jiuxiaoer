package catalog

import (
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
)

// NewQueryService 为只需要公开读取能力的调用方构造套餐目录服务。
func NewQueryService(db *gorm.DB) *Service {
	return &Service{
		repo:    NewRepository(db),
		idStore: idempotency.NewStore(db),
		now:     time.Now,
	}
}
