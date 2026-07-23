package riderapplication

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct {
	service *Service
}

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterPublicRoutes 注册公开数据 Routes。
func RegisterPublicRoutes(api *gin.RouterGroup, handler *Handler) {
	api.POST("/rider-applications", handler.Submit)
	api.POST("/auth/rider-application/sms-login", handler.Login)
}

// RegisterApplicantRoutes 注册申请人 Routes。
func RegisterApplicantRoutes(api *gin.RouterGroup, handler *Handler) {
	group := api.Group("/rider-applications")
	group.Use(handler.ApplicationAuthRequired())
	group.GET("/me", handler.GetOwn)
	group.PATCH("/me", handler.UpdateOwn)
	group.POST("/me/resubmit", handler.Resubmit)
}

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(admin *gin.RouterGroup, handler *Handler) {
	admin.GET("/rider-applications", handler.List)
	admin.GET("/rider-applications/:id", handler.Detail)
	admin.POST("/rider-applications/:id/review", handler.Review)
}

// Submit 处理Submit相关逻辑。
func (h *Handler) Submit(c *gin.Context) {
	var req SubmitRequest
	if err := strictJSON(c, &req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	result, err := h.service.Submit(c.Request.Context(), c.ClientIP(), c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.WithStatus(c, http.StatusCreated, result)
}

// Login 处理Login相关逻辑。
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if err := strictJSON(c, &req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	result, err := h.service.Login(c.Request.Context(), c.ClientIP(), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, result)
}

// GetOwn 获取Own。
func (h *Handler) GetOwn(c *gin.Context) {
	claims, ok := applicationClaims(c)
	if !ok {
		return
	}
	result, err := h.service.GetOwn(c.Request.Context(), claims)
	writeResult(c, result, err)
}

// UpdateOwn 更新Own。
func (h *Handler) UpdateOwn(c *gin.Context) {
	claims, ok := applicationClaims(c)
	if !ok {
		return
	}
	var req UpdateRequest
	if err := strictJSON(c, &req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	result, err := h.service.UpdateOwn(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	writeResult(c, result, err)
}

// Resubmit 处理Resubmit相关逻辑。
func (h *Handler) Resubmit(c *gin.Context) {
	claims, ok := applicationClaims(c)
	if !ok {
		return
	}
	var req VersionRequest
	if err := strictJSON(c, &req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	result, err := h.service.Resubmit(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	writeResult(c, result, err)
}

// List 查询riderapplication列表。
func (h *Handler) List(c *gin.Context) {
	claims, ok := standardClaims(c)
	if !ok {
		return
	}
	pageSize, err := parsePageSize(c.Query("page_size"))
	if err != nil {
		response.Error(c, err)
		return
	}
	result, err := h.service.List(c.Request.Context(), claims, pageSize, c.Query("page_token"), c.Query("filter"), c.Query("order_by"))
	writeResult(c, result, err)
}

// Detail 处理Detail相关逻辑。
func (h *Handler) Detail(c *gin.Context) {
	claims, ok := standardClaims(c)
	if !ok {
		return
	}
	result, err := h.service.Detail(c.Request.Context(), claims, c.Param("id"))
	writeResult(c, result, err)
}

// Review 审核riderapplication。
func (h *Handler) Review(c *gin.Context) {
	claims, ok := standardClaims(c)
	if !ok {
		return
	}
	var req ReviewRequest
	if err := strictJSON(c, &req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	result, err := h.service.Review(c.Request.Context(), claims, c.ClientIP(), c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	writeResult(c, result, err)
}

// ApplicationAuthRequired 返回申请认证 Required。
func (h *Handler) ApplicationAuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "missing bearer token"))
			c.Abort()
			return
		}
		claims, err := h.service.VerifyApplicationToken(c.Request.Context(), raw)
		if err != nil {
			response.Error(c, err)
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(requestctx.WithAccountID(c.Request.Context(), claims.AccountID))
		c.Set("auth_claims", claims)
		c.Next()
	}
}

// strictJSON 返回strict JSON。
func strictJSON(c *gin.Context, target any) error {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errorsIsEOF(err) {
		if err == nil {
			return problem.InvalidArgument("VALIDATION_FAILED", "request body must contain one JSON object")
		}
		return err
	}
	return nil
}

// errorsIsEOF 判断errors Is EOF。
func errorsIsEOF(err error) bool { return err == io.EOF }

// bearerToken 返回bearer 令牌。
func bearerToken(header string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return strings.TrimSpace(parts[1]), true
}

// applicationClaims 返回申请认证声明。
func applicationClaims(c *gin.Context) (*auth.Claims, bool) {
	value, ok := auth.ClaimsFromContext(c)
	if !ok || value.TokenType != "application_access" {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "application authentication required"))
		return nil, false
	}
	return value, true
}

// standardClaims 返回standard 认证声明。
func standardClaims(c *gin.Context) (*auth.Claims, bool) {
	value, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "authentication required"))
		return nil, false
	}
	return value, true
}

// writeResult 写入结果。
func writeResult(c *gin.Context, result any, err error) {
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, result)
}
