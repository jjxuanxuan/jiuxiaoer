package ops

import (
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

func (h *Handler) ListAdminPurchases(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if err := rejectUnknownPackageQuery(
		c,
		"customer_id",
		"purchase_no",
		"status",
		"package_code",
		"issuer_merchant_id",
		"created_from",
		"created_to",
		"page_size",
		"page_token",
	); err != nil {
		response.Error(c, err)
		return
	}
	claims, ok := adminWineTicketClaims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(
		c,
		"admin_wine_ticket_purchases",
		claims.AdminUserID,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	customerID, err := parseOptionalExternalID(c.Query("customer_id"), "customer_id")
	if err != nil {
		response.Error(c, err)
		return
	}
	issuerID, err := parseOptionalExternalID(
		c.Query("issuer_merchant_id"),
		"issuer_merchant_id",
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	createdFrom, err := parseOptionalQueryDateTime(c.Query("created_from"), "created_from")
	if err != nil {
		response.Error(c, err)
		return
	}
	createdTo, err := parseOptionalQueryDateTime(c.Query("created_to"), "created_to")
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListAdminPurchases(
		c.Request.Context(),
		claims,
		query,
		AdminPurchaseFilter{
			CustomerID:       customerID,
			PurchaseNo:       strings.TrimSpace(c.Query("purchase_no")),
			Status:           strings.TrimSpace(c.Query("status")),
			PackageCode:      strings.TrimSpace(c.Query("package_code")),
			IssuerMerchantID: issuerID,
			CreatedFrom:      createdFrom,
			CreatedTo:        createdTo,
		},
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

func (h *Handler) ListAdminLots(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if err := rejectUnknownPackageQuery(
		c,
		"owner_customer_id",
		"lot_no",
		"purchase_no",
		"status",
		"product_id",
		"issuer_merchant_id",
		"expires_before",
		"page_size",
		"page_token",
	); err != nil {
		response.Error(c, err)
		return
	}
	claims, ok := adminWineTicketClaims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(
		c,
		"admin_wine_ticket_lots",
		claims.AdminUserID,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	ownerID, err := parseOptionalExternalID(
		c.Query("owner_customer_id"),
		"owner_customer_id",
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	productID, err := parseOptionalExternalID(c.Query("product_id"), "product_id")
	if err != nil {
		response.Error(c, err)
		return
	}
	issuerID, err := parseOptionalExternalID(
		c.Query("issuer_merchant_id"),
		"issuer_merchant_id",
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	expiresBefore, err := parseOptionalQueryDateTime(
		c.Query("expires_before"),
		"expires_before",
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListAdminLots(
		c.Request.Context(),
		claims,
		query,
		AdminLotFilter{
			OwnerCustomerID:  ownerID,
			LotNo:            strings.TrimSpace(c.Query("lot_no")),
			PurchaseNo:       strings.TrimSpace(c.Query("purchase_no")),
			Status:           strings.TrimSpace(c.Query("status")),
			ProductID:        productID,
			IssuerMerchantID: issuerID,
			ExpiresBefore:    expiresBefore,
		},
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}
