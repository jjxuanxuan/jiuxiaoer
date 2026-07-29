package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

const maxPackageWriteBodyBytes = 64 << 10

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func RegisterPublicRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/wine-tickets/packages", handler.ListPublicPackages)
	router.GET("/wine-tickets/packages/:package_no", handler.GetPublicPackage)
}

func RegisterAdminRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/packages", handler.ListAdminPackages)
	router.POST("/packages", handler.CreateAdminPackage)
	router.GET("/packages/:package_no", handler.GetAdminPackage)
	router.PUT("/packages/:package_no", handler.UpdateAdminPackage)
	router.POST("/packages/:package_no/publish", handler.PublishAdminPackage)
	router.POST(
		"/packages/:package_no/unpublish",
		handler.UnpublishAdminPackage,
	)
}

func (h *Handler) ListPublicPackages(c *gin.Context) {
	if err := rejectUnknownQuery(
		c,
		"package_type",
		"page_size",
		"page_token",
	); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(
		c,
		"wine_ticket_packages",
		"public",
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListPublicPackages(
		c.Request.Context(),
		query,
		c.Query("package_type"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

func (h *Handler) GetPublicPackage(c *gin.Context) {
	item, err := h.service.PublicPackage(
		c.Request.Context(),
		c.Param("package_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func (h *Handler) ListAdminPackages(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownQuery(c, "page_size", "page_token"); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(
		c,
		"wine_ticket_packages",
		"admin",
		claims.AdminUserID,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListAdminPackages(
		c.Request.Context(),
		claims,
		query,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.Page(c, items, next)
}

func (h *Handler) GetAdminPackage(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	item, err := h.service.AdminPackage(
		c.Request.Context(),
		claims,
		c.Param("package_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func (h *Handler) CreateAdminPackage(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req PackageWriteRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.CreateAdminPackage(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		req,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.WithStatus(c, http.StatusCreated, item)
}

func (h *Handler) UpdateAdminPackage(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req PackageWriteRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.UpdateAdminPackage(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.Param("package_no"),
		req,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func (h *Handler) PublishAdminPackage(c *gin.Context) {
	h.packageStatusAction(c, true)
}

func (h *Handler) UnpublishAdminPackage(c *gin.Context) {
	h.packageStatusAction(c, false)
}

func (h *Handler) packageStatusAction(c *gin.Context, publish bool) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req ExpectedVersionRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		response.Error(c, err)
		return
	}
	var (
		item AdminPackageDTO
		err  error
	)
	if publish {
		item, err = h.service.PublishAdminPackage(
			c.Request.Context(),
			claims,
			c.Request.Method,
			c.FullPath(),
			c.GetHeader("Idempotency-Key"),
			c.Param("package_no"),
			req,
		)
	} else {
		item, err = h.service.UnpublishAdminPackage(
			c.Request.Context(),
			claims,
			c.Request.Method,
			c.FullPath(),
			c.GetHeader("Idempotency-Key"),
			c.Param("package_no"),
			req,
		)
	}
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func adminClaims(c *gin.Context) (*auth.Claims, bool) {
	c.Header("Cache-Control", "private, no-store")
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(
			c,
			problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"),
		)
		return nil, false
	}
	return claims, true
}

func rejectUnknownQuery(c *gin.Context, allowed ...string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	for key := range c.Request.URL.Query() {
		if _, ok := allow[key]; !ok {
			return problem.InvalidArgument(
				"VALIDATION_INVALID_QUERY",
				"unknown query parameter: "+key,
			)
		}
	}
	return nil
}

func decodeStrictJSON(c *gin.Context, out any) error {
	reader := http.MaxBytesReader(
		c.Writer,
		c.Request.Body,
		maxPackageWriteBodyBytes,
	)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body is too large or unreadable",
		)
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must be a JSON object",
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			safeJSONError(err),
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must contain exactly one JSON object",
		)
	}
	return nil
}

func safeJSONError(err error) string {
	message := err.Error()
	if strings.Contains(message, "unknown field") ||
		strings.Contains(message, "cannot unmarshal") ||
		strings.Contains(message, "invalid character") ||
		strings.Contains(message, "unexpected EOF") {
		return message
	}
	return "invalid JSON request body"
}
