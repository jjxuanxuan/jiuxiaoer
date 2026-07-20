package aftersale

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
func RegisterCustomerRoutes(g *gin.RouterGroup, h *Handler) {
	g.POST("", h.Create)
	g.GET("", h.ListCustomer)
	g.GET("/:id", h.DetailCustomer)
	g.POST("/:id/evidence", h.AddEvidence)
	g.POST("/:id/withdraw", h.Withdraw)
	g.POST("/:id/appeal", h.Appeal)
}

// RegisterStoreRoutes 注册门店 Routes。
func RegisterStoreRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("", h.ListStore)
	g.GET("/:id", h.DetailStore)
	g.POST("/:id/review", h.ReviewStore)
	g.POST("/:id/return-receipts", h.ReceiveReturn)
	g.POST("/:id/replacements", h.ReserveReplacement)
}

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("", h.ListAdmin)
	g.GET("/:id", h.DetailAdmin)
	g.POST("/:id/review", h.ReviewAdmin)
}

// claims 返回认证声明。
func claims(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return nil, false
	}
	return v, true
}

// bind 绑定售后。
func bind(c *gin.Context, v any) bool {
	if e := c.ShouldBindJSON(v); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return false
	}
	return true
}

// listQuery 查询查询列表。
func listQuery(c *gin.Context) (ListQuery, error) {
	query, err := pagination.FromGin(c)
	return ListQuery{Query: query, Status: c.Query("status"), Type: c.Query("type")}, err
}

// Create 创建售后。
func (h *Handler) Create(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	var req CreateReq
	if !bind(c, &req) {
		return
	}
	v, e := h.service.Create(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// ListCustomer 查询用户列表。
func (h *Handler) ListCustomer(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	q, e := listQuery(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	v, next, e := h.service.ListCustomer(c.Request.Context(), cl, q)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, v, next)
}

// ListStore 查询门店列表。
func (h *Handler) ListStore(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	q, e := listQuery(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	v, next, e := h.service.ListStore(c.Request.Context(), cl, q)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, v, next)
}

// ListAdmin 查询管理端列表。
func (h *Handler) ListAdmin(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	q, e := listQuery(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	v, next, e := h.service.ListAdmin(c.Request.Context(), cl, q)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, v, next)
}

// DetailCustomer 处理Detail 用户相关逻辑。
func (h *Handler) DetailCustomer(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	v, e := h.service.DetailCustomer(c.Request.Context(), cl, c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// DetailStore 处理Detail 门店相关逻辑。
func (h *Handler) DetailStore(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	v, e := h.service.DetailStore(c.Request.Context(), cl, c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// DetailAdmin 处理Detail 管理端相关逻辑。
func (h *Handler) DetailAdmin(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	v, e := h.service.DetailAdmin(c.Request.Context(), cl, c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// AddEvidence 添加Evidence。
func (h *Handler) AddEvidence(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	var req EvidenceReq
	if !bind(c, &req) {
		return
	}
	v, e := h.service.AddEvidence(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// Withdraw 处理Withdraw相关逻辑。
func (h *Handler) Withdraw(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	var req WithdrawReq
	if !bind(c, &req) {
		return
	}
	v, e := h.service.Withdraw(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// Appeal 处理Appeal相关逻辑。
func (h *Handler) Appeal(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	var req AppealReq
	if !bind(c, &req) {
		return
	}
	v, e := h.service.Appeal(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// ReviewStore 审核门店。
func (h *Handler) ReviewStore(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	var req ReviewReq
	if !bind(c, &req) {
		return
	}
	v, e := h.service.ReviewStore(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// ReviewAdmin 审核管理端。
func (h *Handler) ReviewAdmin(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	var req ReviewReq
	if !bind(c, &req) {
		return
	}
	v, e := h.service.ReviewAdmin(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, v)
}

// ReceiveReturn 接收并处理Return。
func (h *Handler) ReceiveReturn(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	var req ReturnReceiptReq
	if !bind(c, &req) {
		return
	}
	v, err := h.service.ReceiveReturn(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// ReserveReplacement 预留Replacement。
func (h *Handler) ReserveReplacement(c *gin.Context) {
	cl, ok := claims(c)
	if !ok {
		return
	}
	var req ReplacementReq
	if !bind(c, &req) {
		return
	}
	v, err := h.service.ReserveReplacement(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}
