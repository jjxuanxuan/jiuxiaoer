package purchase

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type serviceCore struct {
	repo        *Repository
	idStore     *idempotency.Store
	ids         *snowflake.Generator
	assets      *core.AssetService
	wechatAppID string
	now         func() time.Time
}

type Service struct {
	*serviceCore
	paymentService *order.Service
}

func NewService(db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{
		serviceCore: &serviceCore{
			repo:    NewRepository(db),
			idStore: idempotency.NewStore(db),
			ids:     ids,
			assets:  core.NewAssetService(ids),
			now:     time.Now,
		},
	}
}

func (s *Service) WithPaymentService(paymentService *order.Service) *Service {
	s.paymentService = paymentService
	return s
}

func (s *Service) WithWeChatAppID(appID string) *Service {
	s.wechatAppID = strings.TrimSpace(appID)
	return s
}

func (s *Service) WithNow(now func() time.Time) *Service {
	if now != nil {
		s.now = now
	}
	return s
}

func (c *serviceCore) claimIdempotencyWithID(
	ctx context.Context,
	tx *gorm.DB,
	claimID uint64,
	actorType string,
	actorID uint64,
	method string,
	path string,
	key string,
	requestHash string,
) (bool, error) {
	return c.idStore.StartAt(
		ctx,
		tx,
		claimID,
		actorType,
		actorID,
		method,
		path,
		key,
		requestHash,
		c.now(),
	)
}

func (c *serviceCore) cachedResponse(
	ctx context.Context,
	tx *gorm.DB,
	actorType string,
	actorID uint64,
	path string,
	key string,
	out any,
) error {
	found, err := c.idStore.CachedResponse(
		ctx,
		tx,
		actorType,
		actorID,
		path,
		key,
		out,
	)
	if err != nil {
		return err
	}
	if !found {
		return problem.Conflict(
			"IDEMPOTENCY_IN_PROGRESS",
			"request with the same idempotency key is still processing",
		)
	}
	return nil
}

func (c *serviceCore) nowShanghai() time.Time {
	return core.NowShanghai(c.now)
}

func (s *Service) validateSettlementForPublish(
	ctx context.Context,
	tx *gorm.DB,
	row catalog.Package,
) error {
	relation, err := s.repo.SettlementRelation(ctx, tx, row)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.Conflict(
			"WT_PACKAGE_NOT_AVAILABLE",
			"settlement merchant, shop, shop product, or product does not exist",
		)
	}
	if err != nil {
		return err
	}
	if relation.MerchantID != row.IssuerMerchantID ||
		relation.ShopID != row.SettlementShopID ||
		relation.ShopMerchantID != row.IssuerMerchantID ||
		relation.ShopProductID != row.SettlementShopProductID ||
		relation.ShopProductMerchant != row.IssuerMerchantID ||
		relation.ShopProductShop != row.SettlementShopID ||
		relation.ShopProductProduct != row.ProductID ||
		relation.ProductID != row.ProductID {
		return problem.Conflict(
			"WT_PACKAGE_NOT_AVAILABLE",
			"settlement merchant, shop, shop product, and product relationship is invalid",
		)
	}
	if relation.MerchantStatus != "active" ||
		relation.MerchantReviewStatus != "approved" ||
		relation.ShopStatus != "active" ||
		relation.ShopBusinessStatus != "open" ||
		relation.ShopProductStatus != "on_sale" ||
		relation.ProductStatus != "on_sale" ||
		relation.ProductCategoryStatus != "active" ||
		!relation.ProductAgeRestricted {
		return problem.Conflict(
			"WT_PACKAGE_NOT_AVAILABLE",
			"settlement configuration is not eligible for wine ticket purchase",
		)
	}
	return nil
}
