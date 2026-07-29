package gift

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

const (
	giftTokenHeader      = "X-Wine-Gift-Token"
	maxGiftWriteBodySize = 64 << 10
)

type GiftHandler struct {
	service *GiftService
}

func NewGiftHandler(service *GiftService) *GiftHandler {
	return &GiftHandler{service: service}
}

// RegisterGiftPublicRoutes 接收 /api/v1 路由组。
// 预览是唯一匿名访问酒票资产的接口，始终返回 private/no-store。
func RegisterGiftPublicRoutes(router *gin.RouterGroup, handler *GiftHandler) {
	router.GET("/wine-tickets/gift-claims/preview", handler.Preview)
}

// RegisterGiftCustomerRoutes 暴露完整的已认证礼赠接口。
// 分阶段发布的装配入口应使用细粒度注册方法，确保关闭新转赠后，
// 历史记录仍可查询，赠送方仍可取消已经冻结的礼赠。
func RegisterGiftCustomerRoutes(router *gin.RouterGroup, handler *GiftHandler) {
	RegisterGiftContinuityRoutes(router, handler)
	RegisterGiftCreationRoutes(router, handler)
}

// RegisterGiftContinuityRoutes 在停止新的转赠和领取操作时，
// 仍保留读取及安全恢复来源权益的操作。
func RegisterGiftContinuityRoutes(
	router *gin.RouterGroup,
	handler *GiftHandler,
) {
	router.GET("/wine-tickets/gifts", handler.List)
	router.GET("/wine-tickets/gifts/:gift_no", handler.Detail)
	router.POST("/wine-tickets/gifts/:gift_no/cancel", handler.Cancel)
	router.POST("/wine-tickets/gift-claims", handler.Claim)
}

// RegisterGiftCreationRoutes 仅暴露创建新转赠或新分享令牌的操作。
// 领取仍属于连续性操作，因为关闭创建开关时令牌可能已经分享出去。
func RegisterGiftCreationRoutes(
	router *gin.RouterGroup,
	handler *GiftHandler,
) {
	router.POST("/wine-tickets/gifts", handler.Create)
	router.POST("/wine-tickets/gifts/:gift_no/share-tokens", handler.CreateShareToken)
}

func (h *GiftHandler) List(c *gin.Context) {
	giftNoStore(c)
	claims, ok := giftClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownGiftQuery(c, "direction", "status", "page_size", "page_token"); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, "wine_ticket_gifts", claims.CustomerID)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.List(
		c.Request.Context(),
		claims,
		query,
		c.Query("direction"),
		c.Query("status"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	giftNoStore(c)
	response.Page(c, items, next)
}

func (h *GiftHandler) Create(c *gin.Context) {
	giftNoStore(c)
	claims, ok := giftClaims(c)
	if !ok {
		return
	}
	var request GiftCreateRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Create(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		request,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	giftNoStore(c)
	response.WithStatus(c, http.StatusCreated, item)
}

func (h *GiftHandler) Detail(c *gin.Context) {
	giftNoStore(c)
	claims, ok := giftClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownGiftQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Detail(c.Request.Context(), claims, c.Param("gift_no"))
	if err != nil {
		response.Error(c, err)
		return
	}
	giftNoStore(c)
	response.OK(c, item)
}

func (h *GiftHandler) CreateShareToken(c *gin.Context) {
	giftNoStore(c)
	claims, ok := giftClaims(c)
	if !ok {
		return
	}
	var request GiftShareTokenRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.CreateShareToken(
		c.Request.Context(),
		claims,
		c.Param("gift_no"),
		request,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.WithStatus(c, http.StatusCreated, item)
}

func (h *GiftHandler) Cancel(c *gin.Context) {
	giftNoStore(c)
	claims, ok := giftClaims(c)
	if !ok {
		return
	}
	var request GiftExpectedVersionRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Cancel(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.Param("gift_no"),
		request,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	giftNoStore(c)
	response.OK(c, item)
}

func (h *GiftHandler) Preview(c *gin.Context) {
	// 在任何校验前设置，确保即使所有敏感令牌共用同一 URL，
	// 200、404 及其他失败响应也能相互隔离缓存。
	giftNoStore(c)
	if err := rejectUnknownGiftQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Preview(c.Request.Context(), c.GetHeader(giftTokenHeader))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func (h *GiftHandler) Claim(c *gin.Context) {
	giftNoStore(c)
	claims, ok := giftClaims(c)
	if !ok {
		return
	}
	var request GiftClaimRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Claim(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.GetHeader(giftTokenHeader),
		request,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func giftClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		giftNoStore(c)
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return nil, false
	}
	return claims, true
}

func rejectUnknownGiftQuery(c *gin.Context, allowed ...string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	for key := range c.Request.URL.Query() {
		if _, ok := allow[key]; !ok {
			return problem.InvalidArgument("VALIDATION_INVALID_QUERY", "unknown query parameter: "+key)
		}
	}
	return nil
}

func giftNoStore(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
}

func decodeStrictJSON(c *gin.Context, out any) error {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxGiftWriteBodySize)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return problem.InvalidArgument("VALIDATION_FAILED", "request body is too large or unreadable")
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return problem.InvalidArgument("VALIDATION_FAILED", "request body must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return problem.InvalidArgument("VALIDATION_FAILED", safeJSONError(err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return problem.InvalidArgument("VALIDATION_FAILED", "request body must contain exactly one JSON object")
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
