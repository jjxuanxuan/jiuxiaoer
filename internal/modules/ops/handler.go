package ops

import (
	"github.com/gin-gonic/gin"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(s *Service) *Handler { return &Handler{service: s} }

// RegisterRoutes 注册Routes。
func RegisterRoutes(g *gin.RouterGroup, h *Handler) {
	g.POST("/orders/:id/cancel", h.Cancel)
	g.POST("/deliveries/:id/assign", h.Assign)
	g.POST("/deliveries/:id/reassign", h.Reassign)
	g.POST("/deliveries/:id/force-complete", h.ForceComplete)
	g.GET("/deliveries/:id/assignments", h.Assignments)
}

// oc 返回运营控制器。
func oc(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
	}
	return v, ok
}

// bind 绑定运营。
func bind(c *gin.Context, v any) bool {
	if e := c.ShouldBindJSON(v); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return false
	}
	return true
}

// Cancel 取消运营。
func (h *Handler) Cancel(c *gin.Context) {
	v, ok := oc(c)
	if !ok {
		return
	}
	var r CancelReq
	if !bind(c, &r) {
		return
	}
	x, e := h.service.Cancel(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	finish(c, x, e)
}

// Assign 分配运营。
func (h *Handler) Assign(c *gin.Context) {
	v, ok := oc(c)
	if !ok {
		return
	}
	var r AssignmentReq
	if !bind(c, &r) {
		return
	}
	x, e := h.service.Assign(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	finish(c, x, e)
}

// Reassign 重新分配运营。
func (h *Handler) Reassign(c *gin.Context) {
	v, ok := oc(c)
	if !ok {
		return
	}
	var r AssignmentReq
	if !bind(c, &r) {
		return
	}
	x, e := h.service.Reassign(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	finish(c, x, e)
}

// ForceComplete 强制执行Complete。
func (h *Handler) ForceComplete(c *gin.Context) {
	v, ok := oc(c)
	if !ok {
		return
	}
	var r ForceCompleteReq
	if !bind(c, &r) {
		return
	}
	x, e := h.service.ForceComplete(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	finish(c, x, e)
}

// Assignments 处理Assignments相关逻辑。
func (h *Handler) Assignments(c *gin.Context) {
	v, ok := oc(c)
	if !ok {
		return
	}
	x, e := h.service.Assignments(c.Request.Context(), v, c.Param("id"))
	finish(c, x, e)
}

// finish 完成运营。
func finish(c *gin.Context, x any, e error) {
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}
