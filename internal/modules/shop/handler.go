package shop

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
	service   *Service
	lbsMode   string
	locations *customerlocation.Service
}

func (h *Handler) WithLocationContexts(mode string, locations *customerlocation.Service) *Handler {
	h.lbsMode, h.locations = mode, locations
	return h
}

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes 注册Routes。
func RegisterRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/shops", handler.ListPublicShops)
}

// ListPublicShops 查询公开数据 Shops列表。
func (h *Handler) ListPublicShops(c *gin.Context) {
	pageQuery, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	contextID := c.GetHeader("X-Location-Context")
	if h.lbsMode == "enforce" {
		if contextID == "" || h.locations == nil {
			response.Error(c, problem.New(422, "LOCATION_CONTEXT_REQUIRED", "Unprocessable Entity", "X-Location-Context is required"))
			return
		}
		if c.Query("city_code") != "" || c.Query("lat") != "" || c.Query("lng") != "" {
			response.Error(c, problem.Conflict("LOCATION_CONTEXT_CONFLICT", "legacy location query conflicts with location context"))
			return
		}
	}
	if contextID != "" && h.locations != nil && (h.lbsMode == "observe" || h.lbsMode == "enforce") {
		customerID := ""
		if claims, ok := auth.ClaimsFromContext(c); ok && claims.AccountType == "customer" {
			customerID = claims.CustomerID
		}
		actor, actorErr := h.locations.BuildActor(customerID, c.GetHeader(h.locations.SessionHeader()))
		if actorErr != nil {
			if h.lbsMode == "enforce" {
				response.Error(c, actorErr)
				return
			}
		} else {
			location, locationErr := h.locations.GetContext(c.Request.Context(), actor, contextID)
			if locationErr != nil {
				if h.lbsMode == "enforce" {
					response.Error(c, locationErr)
					return
				}
			} else {
				if h.lbsMode == "observe" {
					h.locations.ObserveReadComparison("shops", c.Query("city_code"), "", location)
				}
				if location.LocationLevel == "city" {
					response.Page(c, []any{}, "")
					return
				}
				start := pageQuery.Offset
				if start > len(location.CandidateShops) {
					start = len(location.CandidateShops)
				}
				end := start + pageQuery.PageSize
				if end > len(location.CandidateShops) {
					end = len(location.CandidateShops)
				}
				response.Page(c, location.CandidateShops[start:end], "")
				return
			}
		}
	}

	lat, lng, err := shopLocationQuery(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, nextPageToken, err := h.service.ListPublicShops(c.Request.Context(), ListQuery{
		Query:    pageQuery,
		City:     c.Query("city"),
		District: c.Query("district"),
		Keyword:  c.Query("keyword"),
		CityCode: c.Query("city_code"), Latitude: lat, Longitude: lng,
	})
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

// shopLocationQuery 返回门店 Location 查询。
func shopLocationQuery(c *gin.Context) (*float64, *float64, error) {
	latRaw, lngRaw := c.Query("lat"), c.Query("lng")
	if latRaw == "" && lngRaw == "" {
		return nil, nil, nil
	}
	if latRaw == "" || lngRaw == "" {
		return nil, nil, problem.New(422, "LOCATION_REQUIRED", "Unprocessable Entity", "lat and lng are required together")
	}
	lat, err := strconv.ParseFloat(latRaw, 64)
	if err != nil || lat < -90 || lat > 90 {
		return nil, nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid lat")
	}
	lng, err := strconv.ParseFloat(lngRaw, 64)
	if err != nil || lng < -180 || lng > 180 {
		return nil, nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid lng")
	}
	return &lat, &lng, nil
}
