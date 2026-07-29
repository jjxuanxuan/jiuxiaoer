package ops

import (
	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

func (h *Handler) ListAdminExceptions(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if err := rejectUnknownPackageQuery(
		c,
		"status",
		"severity",
		"page_size",
		"page_token",
	); err != nil {
		response.Error(c, err)
		return
	}
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(
		c,
		"admin_wine_ticket_exceptions",
		claims.AdminUserID,
		c.Query("status"),
		c.Query("severity"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListAdminExceptions(
		c.Request.Context(),
		claims,
		query,
		ExceptionAdminFilter{
			Status:   c.Query("status"),
			Severity: c.Query("severity"),
		},
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

func (h *Handler) GetAdminException(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	item, err := h.service.AdminException(
		c.Request.Context(),
		claims,
		c.Param("exception_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func (h *Handler) ResolveAdminException(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var request ExceptionResolutionRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.ResolveException(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.Param("exception_no"),
		request,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}
