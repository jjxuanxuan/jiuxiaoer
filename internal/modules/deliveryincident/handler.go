package deliveryincident

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func RegisterRiderRoutes(group *gin.RouterGroup, handler *Handler) {
	group.POST("/orders/:id/incidents", handler.Create)
	group.GET("/orders/:id/incidents", handler.RiderList)
	group.GET("/incidents/:id", handler.RiderDetail)
	group.POST("/incidents/:id/evidence", handler.AddEvidence)
	group.GET("/incidents/:id/evidence/:evidence_id/view", handler.RiderEvidenceView)
}

func RegisterStoreRoutes(group *gin.RouterGroup, handler *Handler) {
	group.GET("/delivery-incidents", handler.StoreList)
	group.GET("/delivery-incidents/:id", handler.StoreDetail)
	group.GET("/delivery-incidents/:id/evidence/:evidence_id/view", handler.StoreEvidenceView)
}

func RegisterAdminRoutes(group *gin.RouterGroup, handler *Handler) {
	group.GET("/delivery-incidents", handler.AdminList)
	group.GET("/delivery-incidents/:id", handler.AdminDetail)
	group.GET("/delivery-incidents/:id/evidence/:evidence_id/view", handler.AdminEvidenceView)
	group.POST("/delivery-incidents/:id/acknowledge", handler.Acknowledge)
	group.POST("/delivery-incidents/:id/resolve", handler.Resolve)
	group.POST("/delivery-incidents/:id/reject", handler.Reject)
}

func (h *Handler) Create(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	var req CreateReq
	if !bindIncident(c, &req) {
		h.service.AuditInvalidRequest(c.Request.Context(), claims, c.Request.Method, c.FullPath(), "incident.report", "delivery_order", c.Param("id"))
		return
	}
	item, err := h.service.Create(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.WithStatus(c, 201, item)
}

func (h *Handler) RiderList(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	query, filters, err := incidentQuery(c, false)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.RiderList(c.Request.Context(), claims, c.Param("id"), query, filters)
	finishPage(c, items, next, err)
}

func (h *Handler) RiderDetail(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	item, err := h.service.RiderDetail(c.Request.Context(), claims, c.Param("id"))
	finishIncident(c, item, err)
}

func (h *Handler) AddEvidence(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	var req AddEvidenceReq
	if !bindIncident(c, &req) {
		h.service.AuditInvalidRequest(c.Request.Context(), claims, c.Request.Method, c.FullPath(), "incident.evidence_add", "delivery_incident", c.Param("id"))
		return
	}
	item, err := h.service.AddEvidence(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	finishIncident(c, item, err)
}

func (h *Handler) RiderEvidenceView(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	item, err := h.service.RiderEvidenceView(c.Request.Context(), claims, c.Param("id"), c.Param("evidence_id"))
	finishEvidenceView(c, item, err)
}

func (h *Handler) StoreList(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	query, filters, err := incidentQuery(c, false)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.StoreList(c.Request.Context(), claims, query, filters)
	finishPage(c, items, next, err)
}

func (h *Handler) StoreDetail(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	item, err := h.service.StoreDetail(c.Request.Context(), claims, c.Param("id"))
	finishIncident(c, item, err)
}

func (h *Handler) StoreEvidenceView(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	item, err := h.service.StoreEvidenceView(c.Request.Context(), claims, c.Param("id"), c.Param("evidence_id"))
	finishEvidenceView(c, item, err)
}

func (h *Handler) AdminList(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	query, filters, err := incidentQuery(c, true)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.AdminList(c.Request.Context(), claims, query, filters)
	finishPage(c, items, next, err)
}

func (h *Handler) AdminDetail(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	item, err := h.service.AdminDetail(c.Request.Context(), claims, c.Param("id"))
	finishIncident(c, item, err)
}

func (h *Handler) AdminEvidenceView(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	item, err := h.service.AdminEvidenceView(c.Request.Context(), claims, c.Param("id"), c.Param("evidence_id"))
	finishEvidenceView(c, item, err)
}

func (h *Handler) Acknowledge(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	var req AcknowledgeReq
	if !bindIncident(c, &req) {
		h.service.AuditInvalidRequest(c.Request.Context(), claims, c.Request.Method, c.FullPath(), "incident.acknowledge", "delivery_incident", c.Param("id"))
		return
	}
	item, err := h.service.Acknowledge(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	finishIncident(c, item, err)
}

func (h *Handler) Resolve(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	var req ResolveReq
	if !bindIncident(c, &req) {
		h.service.AuditInvalidRequest(c.Request.Context(), claims, c.Request.Method, c.FullPath(), "incident.resolve", "delivery_incident", c.Param("id"))
		return
	}
	item, err := h.service.Resolve(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	finishIncident(c, item, err)
}

func (h *Handler) Reject(c *gin.Context) {
	claims, ok := incidentClaims(c)
	if !ok {
		return
	}
	var req RejectReq
	if !bindIncident(c, &req) {
		h.service.AuditInvalidRequest(c.Request.Context(), claims, c.Request.Method, c.FullPath(), "incident.reject", "delivery_incident", c.Param("id"))
		return
	}
	item, err := h.service.Reject(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	finishIncident(c, item, err)
}

func incidentClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
	}
	return claims, ok
}

func bindIncident(c *gin.Context, target any) bool {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", "request body must contain exactly one JSON object"))
		return false
	}
	if err := binding.Validator.ValidateStruct(target); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return false
	}
	return true
}

func incidentQuery(c *gin.Context, admin bool) (pagination.Query, ListFilters, error) {
	query, err := pagination.FromGin(c)
	if err != nil {
		return pagination.Query{}, ListFilters{}, err
	}
	if c.Query("page_size") == "" {
		query.PageSize = 30
	}
	filters := ListFilters{Type: strings.TrimSpace(c.Query("type")), Status: strings.TrimSpace(c.Query("status")), Stage: strings.TrimSpace(c.Query("stage"))}
	if filters.Type != "" && filters.Type != TypeOutOfStock && filters.Type != TypeAlcoholDamaged && filters.Type != TypeCustomerRefused && filters.Type != TypeCustomerUnreachable {
		return pagination.Query{}, ListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid incident type")
	}
	if filters.Status != "" && filters.Status != StatusEvidenceRequired && filters.Status != StatusOpen && filters.Status != StatusAcknowledged && filters.Status != StatusResolved && filters.Status != StatusRejected {
		return pagination.Query{}, ListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid incident status")
	}
	if filters.Stage != "" && filters.Stage != StagePickup && filters.Stage != StageDelivery {
		return pagination.Query{}, ListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid incident stage")
	}
	if filters.ReportedFrom, err = queryTime(c.Query("reported_from")); err != nil {
		return pagination.Query{}, ListFilters{}, err
	}
	if filters.ReportedTo, err = queryTime(c.Query("reported_to")); err != nil {
		return pagination.Query{}, ListFilters{}, err
	}
	if filters.ReportedFrom != nil && filters.ReportedTo != nil && filters.ReportedFrom.After(*filters.ReportedTo) {
		return pagination.Query{}, ListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "reported_from must not be after reported_to")
	}
	if admin {
		if raw := strings.TrimSpace(c.Query("shop_id")); raw != "" {
			id, parseErr := parseID(raw)
			if parseErr != nil {
				return pagination.Query{}, ListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid shop_id")
			}
			filters.ShopID = &id
		}
		if raw := strings.TrimSpace(c.Query("rider_id")); raw != "" {
			id, parseErr := parseID(raw)
			if parseErr != nil {
				return pagination.Query{}, ListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid rider_id")
			}
			filters.RiderID = &id
		}
		filters.IncidentNo, filters.OrderNo = strings.TrimSpace(c.Query("incident_no")), strings.TrimSpace(c.Query("order_no"))
		if len(filters.IncidentNo) > 64 || len(filters.OrderNo) > 64 {
			return pagination.Query{}, ListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "incident_no and order_no must not exceed 64 characters")
		}
	}
	return query, filters, nil
}

func queryTime(raw string) (*time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid RFC3339 time filter")
	}
	value = value.UTC()
	return &value, nil
}

func finishIncident(c *gin.Context, item DTO, err error) {
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func finishEvidenceView(c *gin.Context, item EvidenceViewDTO, err error) {
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	response.OK(c, item)
}

func finishPage(c *gin.Context, items []DTO, next string, err error) {
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}
