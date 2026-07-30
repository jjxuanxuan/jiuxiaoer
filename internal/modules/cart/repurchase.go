package cart

import (
	"context"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type repurchaseLocationResolver interface {
	GetContext(context.Context, customerlocation.Actor, string) (customerlocation.LocationContext, error)
}

// WithRepurchase 注入常购清单配置和客户位置上下文解析器。
func (s *Service) WithRepurchase(cfg config.RepurchaseConfig, locations repurchaseLocationResolver) *Service {
	s.repurchase = cfg
	s.locations = locations
	return s
}

// ListFrequentPurchases 返回当前服务门店下可重新购买的历史高频商品。
func (s *Service) ListFrequentPurchases(ctx context.Context, claims *auth.Claims, locationContextID string) (FrequentPurchaseResp, error) {
	if err := s.ensureRepurchaseEnabled(); err != nil {
		return FrequentPurchaseResp{}, err
	}
	customerID, err := customerIDFromClaims(claims, "order:list")
	if err != nil {
		return FrequentPurchaseResp{}, err
	}
	shopID, err := s.resolveRepurchaseShop(ctx, customerID, locationContextID)
	if err != nil {
		return FrequentPurchaseResp{}, err
	}
	since := time.Now().UTC().AddDate(0, 0, -s.repurchase.LookbackDays)
	rows, err := s.repo.ListFrequentPurchases(ctx, customerID, shopID, since, s.repurchase.MaxItems)
	if err != nil {
		return FrequentPurchaseResp{}, err
	}
	resp := FrequentPurchaseResp{
		ShopID:       strconv.FormatUint(shopID, 10),
		LookbackDays: s.repurchase.LookbackDays,
		Items:        make([]FrequentPurchaseDTO, 0, len(rows)),
	}
	for _, row := range rows {
		availability := frequentPurchaseAvailability(row)
		available := availability == "available"
		var unavailableReason *string
		if !available {
			unavailableReason = &availability
		}
		recommendedQuantity := row.LastQuantity
		if recommendedQuantity < 1 {
			recommendedQuantity = 1
		}
		if recommendedQuantity > 99 {
			recommendedQuantity = 99
		}
		if available && recommendedQuantity > row.AvailableQty {
			recommendedQuantity = row.AvailableQty
		}
		shopProductID := ""
		if row.ShopProductID != 0 {
			shopProductID = strconv.FormatUint(row.ShopProductID, 10)
		}
		resp.Items = append(resp.Items, FrequentPurchaseDTO{
			ProductID:           strconv.FormatUint(row.ProductID, 10),
			ShopProductID:       shopProductID,
			ShopID:              strconv.FormatUint(row.ShopID, 10),
			Name:                row.Name,
			BrandName:           stringValue(row.BrandName),
			Spec:                stringValue(row.Spec),
			ImageURL:            stringValue(row.ImageURL),
			PurchaseCount:       row.PurchaseCount,
			PurchasedQuantity:   row.PurchasedQuantity,
			LastQuantity:        row.LastQuantity,
			RecommendedQuantity: recommendedQuantity,
			LastSalePriceAmount: row.LastSalePriceAmount,
			SalePriceAmount:     row.SalePriceAmount,
			LastPurchasedAt:     row.LastPurchasedAt.UTC().Format(time.RFC3339Nano),
			AvailableQty:        row.AvailableQty,
			AvailabilityStatus:  availability,
			Available:           available,
			UnavailableReason:   unavailableReason,
		})
	}
	return resp, nil
}

// Repurchase 将多个历史商品按当前服务门店重新映射并批量放入购物车。
// 单项缺货或下架采用部分成功语义；认证、位置、幂等和门店选择冲突则整体失败。
func (s *Service) Repurchase(ctx context.Context, claims *auth.Claims, method, path, key, locationContextID string, req RepurchaseReq) (RepurchaseResp, error) {
	if err := s.ensureRepurchaseEnabled(); err != nil {
		return RepurchaseResp{}, err
	}
	customerID, err := customerIDFromClaims(claims, "cart:update")
	if err != nil {
		return RepurchaseResp{}, err
	}
	items, err := parseRepurchaseItems(req.Items, s.repurchase.MaxItems)
	if err != nil {
		return RepurchaseResp{}, err
	}
	hashInput := struct {
		LocationContextID string              `json:"location_context_id"`
		Items             []RepurchaseItemReq `json:"items"`
		ReplaceSelection  bool                `json:"replace_selection"`
	}{
		LocationContextID: strings.TrimSpace(locationContextID),
		Items:             req.Items,
		ReplaceSelection:  req.ReplaceSelection,
	}
	requestHash := idempotency.RequestHash(hashInput)
	var replay RepurchaseResp
	if cached, replayErr := s.idStore.ReplayCompleted(ctx, s.repo.DB(), claims.AccountType, customerID, path, key, requestHash, &replay); replayErr != nil {
		return RepurchaseResp{}, replayErr
	} else if cached {
		return replay, nil
	}

	shopID, err := s.resolveRepurchaseShop(ctx, customerID, locationContextID)
	if err != nil {
		return RepurchaseResp{}, err
	}

	var resp RepurchaseResp
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &resp)
			if err != nil {
				return err
			}
			if !cached {
				return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
			}
			return nil
		}

		cartRow, err := s.repo.EnsureCart(ctx, tx, customerID, s.idGen.Next)
		if err != nil {
			return err
		}
		productIDs := make([]uint64, 0, len(items))
		for _, item := range items {
			productIDs = append(productIDs, item.ProductID)
		}
		products, err := s.repo.ShopProductsByProductIDs(ctx, tx, shopID, productIDs)
		if err != nil {
			return err
		}
		productsByID := make(map[uint64]ShopProductRow, len(products))
		for _, product := range products {
			productsByID[product.ProductID] = product
		}

		resp = RepurchaseResp{
			ShopID:  strconv.FormatUint(shopID, 10),
			Results: make([]RepurchaseItemResult, len(items)),
		}
		candidates := make([]repurchaseCandidate, 0, len(items))
		for index, item := range items {
			result := RepurchaseItemResult{
				ProductID:         item.RawID,
				RequestedQuantity: item.Quantity,
				Status:            "skipped",
			}
			product, exists := productsByID[item.ProductID]
			if !exists {
				result.ErrorCode = "PRODUCT_NOT_AVAILABLE_IN_SHOP"
				resp.Results[index] = result
				resp.FailedCount++
				continue
			}
			result.ShopProductID = strconv.FormatUint(product.ShopProductID, 10)
			if err := validateCartProduct(product, 1); err != nil {
				result.ErrorCode = problem.FromError(err).ErrorCode
				resp.Results[index] = result
				resp.FailedCount++
				continue
			}
			currentItem, found, err := s.repo.LockCartItem(ctx, tx, cartRow.ID, product.ShopProductID)
			if err != nil {
				return err
			}
			currentQuantity := currentItem.Quantity
			targetQuantity := max(currentQuantity, item.Quantity)
			if err := validateCartProduct(product, targetQuantity); err != nil {
				result.ErrorCode = problem.FromError(err).ErrorCode
				result.CartQuantity = currentQuantity
				resp.Results[index] = result
				resp.FailedCount++
				continue
			}
			status := "unchanged"
			switch {
			case !found:
				status = "added"
			case targetQuantity > currentQuantity || !currentItem.Selected:
				status = "updated"
			}
			result.Status = status
			result.CartQuantity = targetQuantity
			resp.Results[index] = result
			candidates = append(candidates, repurchaseCandidate{
				Product: product, TargetQuantity: targetQuantity,
			})
		}

		if len(candidates) > 0 {
			conflict, err := s.repo.HasSelectedOtherShop(ctx, tx, cartRow.ID, shopID)
			if err != nil {
				return err
			}
			if conflict && !req.ReplaceSelection {
				return problem.Conflict("CART_SHOP_CONFLICT", "selected cart items belong to another shop")
			}
			if req.ReplaceSelection {
				if err := s.repo.DeselectOtherShops(ctx, tx, cartRow.ID, shopID); err != nil {
					return err
				}
			}
			for _, candidate := range candidates {
				if err := s.repo.SetTargetItemQuantity(ctx, tx, cartRow.ID, candidate.Product, candidate.TargetQuantity, s.idGen.Next); err != nil {
					return err
				}
				resp.SucceededCount++
			}
		}
		resp.Cart, err = s.cartResp(ctx, tx, customerID)
		if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, resp)
	})
	return resp, err
}

func (s *Service) ensureRepurchaseEnabled() error {
	if !s.repurchase.Enabled {
		return problem.New(503, "REPURCHASE_DISABLED", "Service Unavailable", "repurchase is temporarily disabled")
	}
	if s.repurchase.LookbackDays < 1 || s.repurchase.MaxItems < 1 {
		return problem.Internal("repurchase configuration is invalid")
	}
	return nil
}

func (s *Service) resolveRepurchaseShop(ctx context.Context, customerID uint64, contextID string) (uint64, error) {
	contextID = strings.TrimSpace(contextID)
	if contextID == "" {
		return 0, problem.New(422, "LOCATION_CONTEXT_REQUIRED", "Unprocessable Entity", "X-Location-Context is required")
	}
	if s.locations == nil {
		return 0, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context resolver unavailable")
	}
	location, err := s.locations.GetContext(ctx, customerlocation.Actor{
		Type: "customer",
		ID:   strconv.FormatUint(customerID, 10),
	}, contextID)
	if err != nil {
		return 0, err
	}
	if location.LocationLevel == "city" {
		return 0, problem.New(422, "PRECISE_LOCATION_REQUIRED", "Unprocessable Entity", "a precise location is required")
	}
	if !location.Serviceability.Serviceable || location.ServiceShop == nil {
		return 0, problem.New(422, "OUT_OF_SERVICE_AREA", "Unprocessable Entity", "location is not serviceable")
	}
	if !location.ServiceShop.Selectable {
		return 0, problem.Conflict("SERVICE_SHOP_CHANGED", "current service shop is no longer selectable")
	}
	shopID, err := strconv.ParseUint(location.ServiceShop.ID, 10, 64)
	if err != nil || shopID == 0 {
		return 0, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context contains an invalid service shop")
	}
	return shopID, nil
}

func parseRepurchaseItems(values []RepurchaseItemReq, maxItems int) ([]parsedRepurchaseItem, error) {
	if len(values) == 0 {
		return nil, problem.InvalidArgument("REPURCHASE_ITEMS_REQUIRED", "at least one repurchase item is required")
	}
	if len(values) > maxItems {
		return nil, problem.InvalidArgument("REPURCHASE_TOO_MANY_ITEMS", "repurchase item count exceeds the configured limit")
	}
	items := make([]parsedRepurchaseItem, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		productID, err := strconv.ParseUint(value.ProductID, 10, 64)
		if err != nil || productID == 0 || value.Quantity < 1 || value.Quantity > 99 {
			return nil, problem.InvalidArgument("REPURCHASE_ITEM_INVALID", "invalid product_id or quantity")
		}
		if _, exists := seen[productID]; exists {
			return nil, problem.InvalidArgument("REPURCHASE_DUPLICATE_PRODUCT", "repurchase items contain duplicate product_id")
		}
		seen[productID] = struct{}{}
		items = append(items, parsedRepurchaseItem{
			ProductID: productID,
			RawID:     strconv.FormatUint(productID, 10),
			Quantity:  value.Quantity,
		})
	}
	return items, nil
}

func frequentPurchaseAvailability(row FrequentPurchaseRow) string {
	if row.ShopStatus != "active" || row.BusinessStatus != "open" {
		return "shop_closed"
	}
	if row.ShopProductID == 0 {
		return "not_sold_by_shop"
	}
	if row.ProductStatus != "on_sale" || row.CategoryStatus != "active" || row.ShopProductStatus != "on_sale" {
		return "not_on_sale"
	}
	if row.AvailableQty < 1 {
		return "out_of_stock"
	}
	return "available"
}
