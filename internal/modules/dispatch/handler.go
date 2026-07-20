package dispatch

import (
	"context"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterRiderRoutes 注册骑手 Routes。
func RegisterRiderRoutes(router *gin.RouterGroup, handler *Handler) {
	router.PUT("/riders/me/work-status", handler.UpdateWorkStatus)
	router.POST("/riders/me/heartbeat", handler.Heartbeat)
	router.GET("/offers", handler.ListOffers)
	router.POST("/offers/:id/accept", handler.AcceptOffer)
	router.POST("/offers/:id/reject", handler.RejectOffer)
}

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/policies", handler.ListPolicies)
	router.POST("/policies", handler.CreatePolicy)
	router.POST("/policies/:id/validate", handler.ValidatePolicy)
	router.POST("/policies/:id/publish", handler.PublishPolicy)
	router.POST("/policies/:id/retire", handler.RetirePolicy)
	router.GET("/jobs", handler.ListJobs)
	router.GET("/jobs/:id", handler.JobDetail)
	router.POST("/jobs/:id/retry", handler.RetryJob)
}

// UpdateWorkStatus 更新Work 状态。
func (h *Handler) UpdateWorkStatus(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req WorkStatusReq
	if !bind(c, &req) {
		return
	}
	item, err := h.service.UpdateWorkStatus(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	write(c, item, err)
}

// Heartbeat 处理Heartbeat相关逻辑。
func (h *Handler) Heartbeat(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req HeartbeatReq
	if !bind(c, &req) {
		return
	}
	item, err := h.service.Heartbeat(c.Request.Context(), claims, req, c.ClientIP())
	write(c, item, err)
}

// ListOffers 查询Offers列表。
func (h *Handler) ListOffers(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListOffers(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

// AcceptOffer 接受并处理Offer。
func (h *Handler) AcceptOffer(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req OfferActionReq
	if !bind(c, &req) {
		return
	}
	item, err := h.service.AcceptOffer(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	write(c, item, err)
}

// RejectOffer 拒绝Offer。
func (h *Handler) RejectOffer(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req OfferRejectReq
	if !bind(c, &req) {
		return
	}
	item, err := h.service.RejectOffer(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	write(c, item, err)
}

// ListPolicies 查询Policies列表。
func (h *Handler) ListPolicies(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListPolicies(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

// CreatePolicy 创建策略。
func (h *Handler) CreatePolicy(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req PolicyCreateReq
	if !bind(c, &req) {
		return
	}
	item, err := h.service.CreatePolicy(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	write(c, item, err)
}

// ValidatePolicy 校验策略是否合法。
func (h *Handler) ValidatePolicy(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req VersionReq
	if !bind(c, &req) {
		return
	}
	item, err := h.service.ValidatePolicy(c.Request.Context(), claims, c.Param("id"), req)
	write(c, item, err)
}

// PublishPolicy 发布策略。
func (h *Handler) PublishPolicy(c *gin.Context) { h.policyStatus(c, h.service.PublishPolicy) }

// RetirePolicy 处理Retire 策略相关逻辑。
func (h *Handler) RetirePolicy(c *gin.Context) { h.policyStatus(c, h.service.RetirePolicy) }

// policyStatus 处理策略状态相关逻辑。
func (h *Handler) policyStatus(c *gin.Context, fn func(context.Context, *auth.Claims, string, string, string, string, VersionReq) (PolicyDTO, error)) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req VersionReq
	if !bind(c, &req) {
		return
	}
	item, err := fn(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	write(c, item, err)
}

// ListJobs 查询Jobs列表。
func (h *Handler) ListJobs(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListJobs(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

// JobDetail 处理任务 Detail相关逻辑。
func (h *Handler) JobDetail(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	item, err := h.service.JobDetail(c.Request.Context(), claims, c.Param("id"))
	write(c, item, err)
}

// RetryJob 重试任务。
func (h *Handler) RetryJob(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req JobRetryReq
	if !bind(c, &req) {
		return
	}
	item, err := h.service.RetryJob(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	write(c, item, err)
}

// claims 返回认证声明。
func claims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return nil, false
	}
	return claims, true
}

// bind 绑定调度。
func bind(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return false
	}
	return true
}

// write 写入调度。
func write(c *gin.Context, item any, err error) {
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}
