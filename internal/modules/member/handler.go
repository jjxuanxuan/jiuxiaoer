package member

import (
	"github.com/gin-gonic/gin"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(s *Service) *Handler { return &Handler{service: s} }

// RegisterCustomerRoutes 注册用户 Routes。
func RegisterCustomerRoutes(g *gin.RouterGroup, h *Handler) { g.GET("/profile", h.Profile) }

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("/members", h.ListMembers)
	g.GET("/members/:customer_id", h.AdminMember)
	g.GET("/member-tier-rules", h.ListRules)
	g.POST("/member-tier-rules", h.CreateRule)
	g.POST("/member-tier-rules/:id/activate", h.ActivateRule)
}

// memberClaims 返回会员认证声明。
func memberClaims(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return nil, false
	}
	return v, true
}

// memberBind 判断会员绑定。
func memberBind(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return false
	}
	return true
}

// requireMemberIdempotency 校验并确保会员幂等满足要求。
func requireMemberIdempotency(c *gin.Context) bool {
	key := c.GetHeader("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		response.Error(c, problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters"))
		return false
	}
	return true
}

// Profile 处理资料相关逻辑。
func (h *Handler) Profile(c *gin.Context) {
	cl, ok := memberClaims(c)
	if !ok {
		return
	}
	v, err := h.service.Profile(c.Request.Context(), cl)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// ListMembers 查询Members列表。
func (h *Handler) ListMembers(c *gin.Context) {
	cl, ok := memberClaims(c)
	if !ok {
		return
	}
	q, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	v, next, err := h.service.ListMembers(c.Request.Context(), cl, ListQuery{Query: q, TierCode: c.Query("tier_code")})
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, v, next)
}

// AdminMember 处理管理端会员相关逻辑。
func (h *Handler) AdminMember(c *gin.Context) {
	cl, ok := memberClaims(c)
	if !ok {
		return
	}
	v, err := h.service.AdminMember(c.Request.Context(), cl, c.Param("customer_id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// ListRules 查询Rules列表。
func (h *Handler) ListRules(c *gin.Context) {
	cl, ok := memberClaims(c)
	if !ok {
		return
	}
	q, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	v, next, err := h.service.ListRuleSets(c.Request.Context(), cl, q)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, v, next)
}

// CreateRule 创建Rule。
func (h *Handler) CreateRule(c *gin.Context) {
	cl, ok := memberClaims(c)
	if !ok {
		return
	}
	var req RuleSetCreateReq
	if !memberBind(c, &req) {
		return
	}
	v, err := h.service.CreateRuleSet(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// ActivateRule 处理Activate Rule相关逻辑。
func (h *Handler) ActivateRule(c *gin.Context) {
	cl, ok := memberClaims(c)
	if !ok {
		return
	}
	if !requireMemberIdempotency(c) {
		return
	}
	var req ActivateReq
	if c.Request.ContentLength > 0 && !memberBind(c, &req) {
		return
	}
	v, err := h.service.ActivateRuleSet(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.Param("id"), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}
