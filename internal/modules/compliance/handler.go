package compliance

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(s *Service) *Handler { return &Handler{service: s} }

// RegisterCustomerRoutes 注册客户路由。
func RegisterCustomerRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("/me", h.GetMe)
	g.GET("/:id", h.GetRequest)
	g.POST("", h.CreateSession)
}

// RegisterCallbackRoute 注册回调路由。
func RegisterCallbackRoute(api *gin.RouterGroup, h *Handler) {
	api.POST("/identity-verifications/:provider/callbacks", h.Callback)
}

// RegisterAdminRoutes 注册管理端路由。
func RegisterAdminRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("/identity-verifications", h.List)
	g.POST("/identity-verifications/:id/review", h.Review)
}

// cl 返回合规控制器。
func cl(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
	}
	return v, ok
}

// GetMe 获取Me。
func (h *Handler) GetMe(c *gin.Context) {
	v, ok := cl(c)
	if !ok {
		return
	}
	x, e := h.service.GetMe(c.Request.Context(), v)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}

// GetRequest 获取请求。
func (h *Handler) GetRequest(c *gin.Context) {
	v, ok := cl(c)
	if !ok {
		return
	}
	x, e := h.service.GetRequest(c.Request.Context(), v, c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}

// CreateSession 创建会话。
func (h *Handler) CreateSession(c *gin.Context) {
	v, ok := cl(c)
	if !ok {
		return
	}
	var r CreateSessionReq
	if e := c.ShouldBindJSON(&r); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	x, e := h.service.CreateSession(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), r)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.WithStatus(c, http.StatusAccepted, x)
}

// Callback 处理回调相关逻辑。
func (h *Handler) Callback(c *gin.Context) {
	body, e := io.ReadAll(io.LimitReader(c.Request.Body, 64*1024+1))
	if e != nil || len(body) > 64*1024 {
		response.Error(c, problem.RequestTooLarge("IDENTITY_CALLBACK_TOO_LARGE", "identity callback is too large"))
		return
	}
	if e := h.service.Callback(c.Request.Context(), c.Param("provider"), c.Request.Header, body); e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, gin.H{"callback": "accepted"})
}

// List 查询合规列表。
func (h *Handler) List(c *gin.Context) {
	v, ok := cl(c)
	if !ok {
		return
	}
	q, e := pagination.FromGin(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	x, n, e := h.service.List(c.Request.Context(), v, q, c.Query("status"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, x, n)
}

// Review 审核合规。
func (h *Handler) Review(c *gin.Context) {
	v, ok := cl(c)
	if !ok {
		return
	}
	var r ReviewReq
	if e := c.ShouldBindJSON(&r); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	x, e := h.service.Review(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}
