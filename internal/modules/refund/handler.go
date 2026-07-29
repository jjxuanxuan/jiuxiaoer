package refund

import (
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterCallbackRoute 注册回调路由。
func RegisterCallbackRoute(api *gin.RouterGroup, handler *Handler) {
	api.POST("/refunds/:provider/callbacks", handler.Callback)
}

// RegisterAdminRoutes 注册管理端路由。
func RegisterAdminRoutes(group *gin.RouterGroup, handler *Handler) {
	group.GET("", handler.List)
	group.GET("/repair-candidates", handler.RepairCandidates)
	group.GET("/:id", handler.Detail)
	group.POST("/:id/repair", handler.RepairStored)
	group.POST("/:id/retry", handler.Retry)
	group.POST("/:id/reconcile", handler.Reconcile)
	group.POST("/:id/mark-exception", handler.MarkException)
}

// RepairCandidates 为受控存量退款修复手册返回只读的游标式清单。
func (h *Handler) RepairCandidates(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	size := 50
	if value := c.Query("page_size"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", "page_size must be an integer"))
			return
		}
		size = parsed
	}
	var afterID uint64
	if value := c.Query("after_id"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", "after_id must be an unsigned integer"))
			return
		}
		afterID = parsed
	}
	page, err := h.service.RepairCandidates(c.Request.Context(), claims, afterID, size)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, page)
}

// RepairStored 默认预览提供程序查询。调用者必须设置
// apply=true 并提供幂等键来改变本地状态.
func (h *Handler) RepairStored(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req struct {
		Apply bool `json:"apply"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	result, err := h.service.RepairStored(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.Apply)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, result)
}

// Callback 处理回调相关逻辑。
func (h *Handler) Callback(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, paygateway.MaxCallbackBodyBytes+1))
	if err != nil || int64(len(body)) > paygateway.MaxCallbackBodyBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": "FAIL", "message": "callback rejected"})
		return
	}
	if err := h.service.ProcessCallback(c.Request.Context(), c.Param("provider"), c.Request, body); err != nil {
		c.JSON(refundCallbackFailureStatus(err), gin.H{"code": "FAIL", "message": "callback rejected"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "成功"})
}

func refundCallbackFailureStatus(err error) int {
	details := problem.FromError(err)
	switch details.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusRequestEntityTooLarge:
		return details.Status
	case http.StatusNotFound:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// List 查询退款列表。
func (h *Handler) List(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.List(c.Request.Context(), claims, c.Query("status"), query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

// Detail 处理Detail相关逻辑。
func (h *Handler) Detail(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	item, err := h.service.Detail(c.Request.Context(), claims, c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// Retry 重试退款。
func (h *Handler) Retry(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	if err := h.service.Retry(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id")); err != nil {
		response.Error(c, err)
		return
	}
	// 服务商退款为 CLOSED 时会创建新的替代退款单号；待处理或未知退款则安排
	// 查询原单号。两个分支都可准确描述为 scheduled；调用方可刷新退款列表，
	// 查看可能出现的替代关系。
	response.OK(c, gin.H{"status": "scheduled"})
}

// Reconcile 在通过微信支付商户平台处理异常退款后安排服务商查询。
func (h *Handler) Reconcile(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	if err := h.service.Reconcile(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id")); err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, gin.H{"status": "pending"})
}

// MarkException 标记Exception的状态。
func (h *Handler) MarkException(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req struct {
		Reason string `json:"reason" binding:"required,min=3,max=500"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	if err := h.service.MarkException(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.Reason); err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, gin.H{"status": "exception"})
}
