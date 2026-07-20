package asset

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
	g.GET("", h.Summaries)
	g.GET("/:asset_type/transactions", h.CustomerTransactions)
}

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("/asset-transactions", h.AdminTransactions)
	g.GET("/asset-transactions/:id", h.AdminTransaction)
	g.POST("/asset-adjustments", h.CreateAdjustment)
	g.POST("/asset-adjustments/:id/approve", h.ApproveAdjustment)
	g.POST("/asset-adjustments/:id/reject", h.RejectAdjustment)
	g.GET("/asset-reconciliations", h.ListReconciliations)
	g.POST("/asset-reconciliations", h.RunReconciliation)
	g.POST("/asset-reconciliations/:id/repair", h.RepairReconciliation)
}

// assetClaims 返回资产认证声明。
func assetClaims(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return nil, false
	}
	return v, true
}

// assetBind 判断资产绑定。
func assetBind(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return false
	}
	return true
}

// requireAssetIdempotency 校验并确保资产幂等满足要求。
func requireAssetIdempotency(c *gin.Context) bool {
	key := c.GetHeader("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		response.Error(c, problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters"))
		return false
	}
	return true
}

// Summaries 处理摘要列表相关逻辑。
func (h *Handler) Summaries(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	v, err := h.service.Summaries(c.Request.Context(), cl)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// CustomerTransactions 处理用户交易相关逻辑。
func (h *Handler) CustomerTransactions(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	q, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	v, next, err := h.service.ListCustomerTransactions(c.Request.Context(), cl, c.Param("asset_type"), q)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, v, next)
}

// AdminTransactions 处理管理端交易相关逻辑。
func (h *Handler) AdminTransactions(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	q, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	v, next, err := h.service.ListAdminTransactions(c.Request.Context(), cl, ListQuery{Query: q, AssetType: c.Query("asset_type"), SourceType: c.Query("source_type"), Action: c.Query("action")})
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, v, next)
}

// AdminTransaction 处理管理端交易相关逻辑。
func (h *Handler) AdminTransaction(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	v, err := h.service.AdminTransaction(c.Request.Context(), cl, c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// CreateAdjustment 创建调整单。
func (h *Handler) CreateAdjustment(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	var req AdjustmentCreateReq
	if !assetBind(c, &req) {
		return
	}
	v, err := h.service.CreateAdjustment(c.Request.Context(), cl, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// ApproveAdjustment 审批通过调整单。
func (h *Handler) ApproveAdjustment(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	var req AdjustmentReviewReq
	if !assetBind(c, &req) {
		return
	}
	v, err := h.service.ApproveAdjustment(c.Request.Context(), cl, c.Param("id"), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// RejectAdjustment 拒绝调整单。
func (h *Handler) RejectAdjustment(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	if !requireAssetIdempotency(c) {
		return
	}
	var req AdjustmentReviewReq
	if !assetBind(c, &req) {
		return
	}
	v, err := h.service.RejectAdjustment(c.Request.Context(), cl, c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// ListReconciliations 查询Reconciliations列表。
func (h *Handler) ListReconciliations(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	q, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	v, next, err := h.service.ListReconciliations(c.Request.Context(), cl, q)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, v, next)
}

// RunReconciliation 运行对账处理流程。
func (h *Handler) RunReconciliation(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	if !requireAssetIdempotency(c) {
		return
	}
	var req ReconcileReq
	if !assetBind(c, &req) {
		return
	}
	v, err := h.service.RunReconciliation(c.Request.Context(), cl, c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}

// RepairReconciliation 修复对账。
func (h *Handler) RepairReconciliation(c *gin.Context) {
	cl, ok := assetClaims(c)
	if !ok {
		return
	}
	if !requireAssetIdempotency(c) {
		return
	}
	v, err := h.service.RepairReconciliation(c.Request.Context(), cl, c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, v)
}
