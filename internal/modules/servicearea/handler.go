package servicearea

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct {
	service *Service
}

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes 注册Routes。
func RegisterRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/service-shops/resolve", handler.Resolve)
}

// Resolve 处理Resolve相关逻辑。
func (h *Handler) Resolve(c *gin.Context) {
	lat, latErr := strconv.ParseFloat(c.Query("lat"), 64)
	lng, lngErr := strconv.ParseFloat(c.Query("lng"), 64)
	if latErr != nil || lngErr != nil {
		response.Error(c, problem.New(422, "LOCATION_REQUIRED", "Unprocessable Entity", "valid city_code, lat and lng are required"))
		return
	}
	result, err := h.service.Resolve(c.Request.Context(), ResolveInput{
		CityCode: c.Query("city_code"), Latitude: lat, Longitude: lng,
	})
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, result)
}
