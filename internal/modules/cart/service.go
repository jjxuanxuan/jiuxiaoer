package cart

import (
	"context"
	"errors"
	"strconv"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	repo    *Repository
	idStore *idempotency.Store
	idGen   *snowflake.Generator
}

// NewService 负责 C 端购物车写操作，并提供幂等保护。
func NewService(db *gorm.DB, idGen *snowflake.Generator) *Service {
	return &Service{
		repo:    NewRepository(db),
		idStore: idempotency.NewStore(db),
		idGen:   idGen,
	}
}

// GetCart 获取购物车。
func (s *Service) GetCart(ctx context.Context, claims *auth.Claims) (CartResp, error) {
	customerID, err := customerIDFromClaims(claims, "cart:view")
	if err != nil {
		return CartResp{}, err
	}
	return s.cartResp(ctx, s.repo.DB(), customerID)
}

// AddItem 在修改购物车前校验门店商品当前是否可售。
func (s *Service) AddItem(ctx context.Context, claims *auth.Claims, method string, path string, key string, req CartItemAddReq) (CartResp, error) {
	customerID, err := customerIDFromClaims(claims, "cart:update")
	if err != nil {
		return CartResp{}, err
	}

	var resp CartResp
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 重复加购请求返回当前购物车，不会把数量累加两次。
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(req))
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
		shopProductID, err := parseID(req.ShopProductID)
		if err != nil {
			return problem.InvalidArgument("CART_ITEM_INVALID", "invalid shop_product_id")
		}
		productRow, err := s.repo.SaleableShopProduct(ctx, tx, shopProductID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.Conflict("PRODUCT_NOT_ON_SALE", "product not on sale")
		}
		if err != nil {
			return err
		}
		currentQuantity, err := s.repo.CartItemQuantity(ctx, tx, cartRow.ID, shopProductID)
		if err != nil {
			return err
		}
		resultingQuantity := currentQuantity + req.Quantity
		if resultingQuantity > 99 {
			return problem.InvalidArgument("CART_QUANTITY_LIMIT", "cart item quantity exceeds 99")
		}
		if err := validateCartProduct(productRow, resultingQuantity); err != nil {
			return err
		}
		conflict, err := s.repo.HasSelectedOtherShop(ctx, tx, cartRow.ID, productRow.ShopID)
		if err != nil {
			return err
		}
		if conflict {
			return problem.Conflict("CART_SHOP_CONFLICT", "selected cart items must belong to one shop")
		}

		if err := s.repo.AddItem(ctx, tx, cartRow.ID, productRow, req.Quantity, s.idGen.Next); err != nil {
			return err
		}
		resp, err = s.cartResp(ctx, tx, customerID)
		if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, resp)
	})
	return resp, err
}

// SetItemSelection 设置明细选中状态。
func (s *Service) SetItemSelection(ctx context.Context, claims *auth.Claims, method, path, key, itemID string, req CartItemSelectionReq) (CartResp, error) {
	customerID, err := customerIDFromClaims(claims, "cart:update")
	if err != nil {
		return CartResp{}, err
	}
	id, err := parseID(itemID)
	if err != nil {
		return CartResp{}, problem.InvalidArgument("CART_ITEM_INVALID", "invalid cart item id")
	}
	request := struct {
		ID       string `json:"id"`
		Selected bool   `json:"selected"`
	}{itemID, req.Selected}
	return s.cartMutation(ctx, claims, customerID, method, path, key, request, func(tx *gorm.DB) error {
		if _, err := s.repo.EnsureCart(ctx, tx, customerID, s.idGen.Next); err != nil {
			return err
		}
		if err := s.repo.SetItemSelection(ctx, tx, customerID, id, req.Selected); errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("CART_ITEM_NOT_FOUND", "cart item not found")
		} else if err != nil {
			return err
		}
		if !req.Selected {
			return nil
		}
		rows, err := s.repo.ListItems(ctx, tx, customerID)
		if err != nil {
			return err
		}
		var targetShop uint64
		for _, row := range rows {
			if row.ID == id {
				targetShop = row.ShopID
				if availability := cartAvailability(row); availability != "available" {
					return cartUnavailableMutationError(availability)
				}
				break
			}
		}
		if targetShop == 0 {
			return problem.NotFound("CART_ITEM_NOT_FOUND", "cart item not found")
		}
		for _, row := range rows {
			if row.Selected && row.ShopID != targetShop {
				return problem.Conflict("CART_SHOP_CONFLICT", "selected cart items must belong to one shop")
			}
		}
		return nil
	})
}

// SetShopSelection 设置门店选中状态。
func (s *Service) SetShopSelection(ctx context.Context, claims *auth.Claims, method, path, key string, req CartSelectionReq) (CartResp, error) {
	customerID, err := customerIDFromClaims(claims, "cart:update")
	if err != nil {
		return CartResp{}, err
	}
	shopID, err := parseID(req.ShopID)
	if err != nil {
		return CartResp{}, problem.InvalidArgument("CART_ITEM_INVALID", "invalid shop_id")
	}
	return s.cartMutation(ctx, claims, customerID, method, path, key, req, func(tx *gorm.DB) error {
		cartRow, err := s.repo.EnsureCart(ctx, tx, customerID, s.idGen.Next)
		if err != nil {
			return err
		}
		if req.Selected {
			conflict, err := s.repo.HasSelectedOtherShop(ctx, tx, cartRow.ID, shopID)
			if err != nil {
				return err
			}
			if conflict {
				return problem.Conflict("CART_SHOP_CONFLICT", "selected cart items must belong to one shop")
			}
			rows, err := s.repo.ListItems(ctx, tx, customerID)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.ShopID != shopID {
					continue
				}
				if availability := cartAvailability(row); availability != "available" {
					return cartUnavailableMutationError(availability)
				}
			}
		}
		return s.repo.SetShopSelection(ctx, tx, customerID, shopID, req.Selected)
	})
}

// ClearItems 清空明细。
func (s *Service) ClearItems(ctx context.Context, claims *auth.Claims, method, path, key, shopIDRaw string) error {
	customerID, err := customerIDFromClaims(claims, "cart:update")
	if err != nil {
		return err
	}
	var shopID *uint64
	if shopIDRaw != "" {
		value, err := parseID(shopIDRaw)
		if err != nil {
			return problem.InvalidArgument("CART_ITEM_INVALID", "invalid shop_id")
		}
		shopID = &value
	}
	request := map[string]string{"shop_id": shopIDRaw}
	_, err = s.cartMutation(ctx, claims, customerID, method, path, key, request, func(tx *gorm.DB) error {
		if _, err := s.repo.EnsureCart(ctx, tx, customerID, s.idGen.Next); err != nil {
			return err
		}
		return s.repo.ClearItems(ctx, tx, customerID, shopID)
	})
	return err
}

// cartMutation 返回购物车 Mutation。
func (s *Service) cartMutation(ctx context.Context, claims *auth.Claims, customerID uint64, method, path, key string, request any, mutate func(*gorm.DB) error) (CartResp, error) {
	var resp CartResp
	err := s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(request))
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
		if err := mutate(tx); err != nil {
			return err
		}
		resp, err = s.cartResp(ctx, tx, customerID)
		if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, resp)
	})
	return resp, err
}

// UpdateItem 更新明细。
func (s *Service) UpdateItem(ctx context.Context, claims *auth.Claims, method string, path string, key string, itemID string, req CartItemUpdateReq) (CartResp, error) {
	customerID, err := customerIDFromClaims(claims, "cart:update")
	if err != nil {
		return CartResp{}, err
	}
	parsedItemID, err := parseID(itemID)
	if err != nil {
		return CartResp{}, problem.InvalidArgument("CART_ITEM_INVALID", "invalid cart item id")
	}

	var resp CartResp
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		request := struct {
			ID   string            `json:"id"`
			Body CartItemUpdateReq `json:"body"`
		}{itemID, req}
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(request))
		if err != nil {
			return err
		}
		if started {
			item, err := s.repo.LockCustomerCartItem(ctx, tx, customerID, parsedItemID)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.NotFound("CART_ITEM_NOT_FOUND", "cart item not found")
			}
			if err != nil {
				return err
			}
			productRow, err := s.repo.SaleableShopProduct(ctx, tx, item.ShopProductID)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.Conflict("PRODUCT_NOT_ON_SALE", "product not on sale")
			}
			if err != nil {
				return err
			}
			if err := validateCartProduct(productRow, req.Quantity); err != nil {
				return err
			}
			if err := s.repo.UpdateItem(ctx, tx, customerID, parsedItemID, req.Quantity); errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.NotFound("CART_ITEM_NOT_FOUND", "cart item not found")
			} else if err != nil {
				return err
			}
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
		resp, err = s.cartResp(ctx, tx, customerID)
		if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, resp)
	})
	return resp, err
}

// DeleteItem 删除明细。
func (s *Service) DeleteItem(ctx context.Context, claims *auth.Claims, method string, path string, key string, itemID string) error {
	customerID, err := customerIDFromClaims(claims, "cart:update")
	if err != nil {
		return err
	}
	parsedItemID, err := parseID(itemID)
	if err != nil {
		return problem.InvalidArgument("CART_ITEM_INVALID", "invalid cart item id")
	}

	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(map[string]string{"id": itemID}))
		if err != nil {
			return err
		}
		if !started {
			return nil
		}
		if err := s.repo.DeleteItem(ctx, tx, customerID, parsedItemID); errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("CART_ITEM_NOT_FOUND", "cart item not found")
		} else if err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, map[string]any{})
	})
}

// cartResp 根据当前购物车行重新计算总数和总价，不信任客户端金额。
func (s *Service) cartResp(ctx context.Context, db *gorm.DB, customerID uint64) (CartResp, error) {
	rows, err := s.repo.ListItems(ctx, db, customerID)
	if err != nil {
		return CartResp{}, err
	}
	resp := CartResp{Items: make([]CartItemDTO, 0, len(rows))}
	for _, row := range rows {
		totalAmount := int64(row.Quantity) * row.SalePriceAmount
		availability := cartAvailability(row)
		available := availability == "available"
		var unavailableReason *string
		if !available {
			resp.UnavailableCount++
			unavailableReason = &availability
		}
		if row.Selected && available {
			resp.TotalQuantity += row.Quantity
			resp.TotalAmount += totalAmount
		}
		resp.Items = append(resp.Items, CartItemDTO{
			ID:                 strconv.FormatUint(row.ID, 10),
			ShopProductID:      strconv.FormatUint(row.ShopProductID, 10),
			ShopID:             strconv.FormatUint(row.ShopID, 10),
			ProductID:          strconv.FormatUint(row.ProductID, 10),
			Name:               row.Name,
			BrandName:          stringValue(row.BrandName),
			Spec:               stringValue(row.Spec),
			ImageURL:           stringValue(row.ImageURL),
			Quantity:           row.Quantity,
			SalePriceAmount:    row.SalePriceAmount,
			TotalAmount:        totalAmount,
			Selected:           row.Selected,
			AvailabilityStatus: availability,
			Available:          available,
			UnavailableReason:  unavailableReason,
		})
	}
	return resp, nil
}

// cartAvailability 返回购物车 Availability。
func cartAvailability(row CartItemRow) string {
	if row.ShopStatus != "active" || row.BusinessStatus != "open" {
		return "shop_closed"
	}
	if row.ProductStatus != "on_sale" || row.CategoryStatus != "active" || row.ShopProductStatus != "on_sale" {
		return "not_on_sale"
	}
	if row.AvailableQty < row.Quantity {
		return "out_of_stock"
	}
	return "available"
}

// cartUnavailableMutationError maps the stable read-side reason to the same
// write-side business error used by add/update operations.
func cartUnavailableMutationError(reason string) error {
	switch reason {
	case "shop_closed":
		return problem.Conflict("SHOP_CLOSED", "shop is closed")
	case "not_on_sale":
		return problem.Conflict("PRODUCT_NOT_ON_SALE", "product not on sale")
	case "out_of_stock":
		return problem.Conflict("STOCK_NOT_ENOUGH", "stock not enough")
	default:
		return problem.Conflict("CART_ITEM_UNAVAILABLE", "cart item is unavailable")
	}
}

func validateCartProduct(row ShopProductRow, quantity int) error {
	if row.ShopStatus != "active" || row.BusinessStatus != "open" {
		return problem.Conflict("SHOP_CLOSED", "shop is closed")
	}
	if row.ProductStatus != "on_sale" || row.CategoryStatus != "active" || row.ShopProductStatus != "on_sale" {
		return problem.Conflict("PRODUCT_NOT_ON_SALE", "product not on sale")
	}
	if row.AvailableQty < quantity {
		return problem.Conflict("STOCK_NOT_ENOUGH", "stock not enough")
	}
	return nil
}

// customerIDFromClaims 从认证声明中解析并返回用户 ID。
func customerIDFromClaims(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
	}
	if !hasPermission(claims.Permissions, permission) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	return parseID(claims.CustomerID)
}

func hasPermission(permissions []string, required string) bool {
	for _, permission := range permissions {
		if permission == required {
			return true
		}
	}
	return false
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, problem.InvalidArgument("VALIDATION_FAILED", "invalid id")
	}
	return id, nil
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
