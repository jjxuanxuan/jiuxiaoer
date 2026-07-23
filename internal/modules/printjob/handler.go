package printjob

import (
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterStoreRoutes 注册门店 Routes。
func RegisterStoreRoutes(group *gin.RouterGroup, h *Handler) {
	group.GET("/print-settings", h.GetSettings)
	group.POST("/print-settings", h.CreateSettings)
	group.PATCH("/print-settings/:id", h.PatchSettings)
	group.POST("/print-settings/:id/test", h.TestSettings)
	group.GET("/print-tasks", h.ListStoreTasks)
	group.GET("/print-tasks/:id", h.GetTask)
	group.POST("/print-tasks/:id/reprint", h.Reprint)
}

// CreateSettings creates the first configuration for an authorized shop.
func (h *Handler) CreateSettings(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	var req SettingCreateReq
	if e := c.ShouldBindJSON(&req); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	item, e := h.service.CreateSettings(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, item)
}

// TestSettings sends a standard non-PII test page to the configured device.
func (h *Handler) TestSettings(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	item, e := h.service.TestSettings(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, item)
}

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(group *gin.RouterGroup, h *Handler) {
	group.GET("/print-tasks", h.ListAdminTasks)
	group.POST("/print-tasks/:id/retry", h.Retry)
}

// claims 返回认证声明。
func claims(c *gin.Context) (*auth.Claims, bool) {
	value, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
	}
	return value, ok
}

// GetSettings 获取Settings。
func (h *Handler) GetSettings(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	item, e := h.service.GetSettings(c.Request.Context(), v, c.Query("shop_id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, item)
}

// PatchSettings 处理Patch Settings相关逻辑。
func (h *Handler) PatchSettings(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	var req SettingPatchReq
	if e := c.ShouldBindJSON(&req); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	item, e := h.service.PatchSettings(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, item)
}

// ListStoreTasks 查询门店 Tasks列表。
func (h *Handler) ListStoreTasks(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	status, e := printTaskListStatusFromGin(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	q, e := pagination.FromGin(c, merchantPaginationScope(v)...)
	if e != nil {
		response.Error(c, e)
		return
	}
	items, next, e := h.service.ListStoreTasks(c.Request.Context(), v, q, status)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, items, next)
}

// ListAdminTasks 查询管理端 Tasks列表。
func (h *Handler) ListAdminTasks(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	status, e := printTaskListStatusFromGin(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	q, e := pagination.FromGin(c, "admin", v.AdminUserID)
	if e != nil {
		response.Error(c, e)
		return
	}
	items, next, e := h.service.ListAdminTasks(c.Request.Context(), v, q, status)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, items, next)
}

func printTaskListStatusFromGin(c *gin.Context) (string, error) {
	allowed := map[string]struct{}{"page_size": {}, "page_token": {}, "status": {}}
	for key := range c.Request.URL.Query() {
		if _, ok := allowed[key]; !ok {
			return "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "unknown query parameter: "+key)
		}
	}
	status := strings.TrimSpace(c.Query("status"))
	if status == "" {
		return "", nil
	}
	valid := map[string]struct{}{
		"pending": {}, "processing": {}, "querying": {}, "succeeded": {},
		"retry_wait": {}, "dead": {}, "cancelled": {},
	}
	if _, ok := valid[status]; !ok {
		return "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid print task status")
	}
	return status, nil
}

func merchantPaginationScope(claims *auth.Claims) []string {
	shops := append([]string(nil), claims.AuthorizedShopIDs...)
	sort.Strings(shops)
	return []string{"merchant", claims.MerchantID, claims.MerchantUserID, strings.Join(shops, ",")}
}

// GetTask 获取任务。
func (h *Handler) GetTask(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	item, e := h.service.GetTask(c.Request.Context(), v, c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, item)
}

// Reprint 处理Reprint相关逻辑。
func (h *Handler) Reprint(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	var req ReprintReq
	if e := c.ShouldBindJSON(&req); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	item, e := h.service.Reprint(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, item)
}

// Retry 重试printjob。
func (h *Handler) Retry(c *gin.Context) {
	v, ok := claims(c)
	if !ok {
		return
	}
	var req RetryReq
	if e := c.ShouldBindJSON(&req); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	item, e := h.service.Retry(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, item)
}
