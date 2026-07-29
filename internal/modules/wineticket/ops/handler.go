package ops

import "github.com/gin-gonic/gin"

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterAdminRoutes 接收 /api/v1/admin/wine-tickets 路由组，
// 只负责跨子域运营读取及异常处置命令。
func RegisterAdminRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/purchases", handler.ListAdminPurchases)
	router.GET("/lots", handler.ListAdminLots)
	router.GET("/exceptions", handler.ListAdminExceptions)
	router.GET("/exceptions/:exception_no", handler.GetAdminException)
	router.POST(
		"/exceptions/:exception_no/resolution",
		handler.ResolveAdminException,
	)
}
