package product

import (
	"github.com/gin-gonic/gin"
	"strconv"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
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
	router.GET("/categories", handler.ListCategories)
	router.GET("/products", handler.ListProducts)
	router.GET("/products/:id", handler.GetProduct)
}

// ListCategories 查询Categories列表。
func (h *Handler) ListCategories(c *gin.Context) {
	items, err := h.service.ListCategories(c.Request.Context())
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, "")
}

// ListProducts 查询商品列表。
func (h *Handler) ListProducts(c *gin.Context) {
	pageQuery, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	lat, lng, err := locationQuery(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	actor := customerlocation.Actor{}
	if h.service.lbsMode != "off" {
		actor, err = productLocationActor(c, h.service.locations)
		if err != nil {
			if h.service.lbsMode == "observe" {
				actor = customerlocation.Actor{}
			} else {
				response.Error(c, err)
				return
			}
		}
	}
	items, nextPageToken, err := h.service.ListProducts(c.Request.Context(), ListQuery{
		Query:      pageQuery,
		ShopID:     c.Query("shop_id"),
		CategoryID: c.Query("category_id"),
		Keyword:    c.Query("keyword"),
		CityCode:   c.Query("city_code"), Latitude: lat, Longitude: lng,
		LocationContextID: c.GetHeader("X-Location-Context"), LocationActor: actor,
	})
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

// GetProduct 获取商品。
func (h *Handler) GetProduct(c *gin.Context) {
	lat, lng, err := locationQuery(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	actor := customerlocation.Actor{}
	if h.service.lbsMode != "off" {
		actor, err = productLocationActor(c, h.service.locations)
		if err != nil {
			if h.service.lbsMode == "observe" {
				actor = customerlocation.Actor{}
			} else {
				response.Error(c, err)
				return
			}
		}
	}
	item, err := h.service.GetPublicProduct(c.Request.Context(), c.Param("id"), ListQuery{
		ShopID: c.Query("shop_id"), CityCode: c.Query("city_code"), Latitude: lat, Longitude: lng,
		LocationContextID: c.GetHeader("X-Location-Context"), LocationActor: actor,
	})
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func productLocationActor(c *gin.Context, locations *customerlocation.Service) (customerlocation.Actor, error) {
	if locations == nil || c.GetHeader("X-Location-Context") == "" {
		return customerlocation.Actor{}, nil
	}
	customerID := ""
	if claims, ok := auth.ClaimsFromContext(c); ok && claims.AccountType == "customer" {
		customerID = claims.CustomerID
	}
	return locations.BuildActor(customerID, c.GetHeader(locations.SessionHeader()))
}

// locationQuery 返回location 查询。
func locationQuery(c *gin.Context) (*float64, *float64, error) {
	latRaw, lngRaw := c.Query("lat"), c.Query("lng")
	if latRaw == "" && lngRaw == "" {
		return nil, nil, nil
	}
	if latRaw == "" || lngRaw == "" {
		return nil, nil, problem.New(422, "LOCATION_REQUIRED", "Unprocessable Entity", "lat and lng are required together")
	}
	lat, err := strconv.ParseFloat(latRaw, 64)
	if err != nil {
		return nil, nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid lat")
	}
	lng, err := strconv.ParseFloat(lngRaw, 64)
	if err != nil {
		return nil, nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid lng")
	}
	return &lat, &lng, nil
}
