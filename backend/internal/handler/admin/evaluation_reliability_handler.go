package admin

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RadarReliabilityHandler exposes the immutable Load Plan management API.
type RadarReliabilityHandler struct {
	repo service.RadarReliabilityRepository
}

func NewRadarReliabilityHandler(repo service.RadarReliabilityRepository) *RadarReliabilityHandler {
	return &RadarReliabilityHandler{repo: repo}
}

func (h *RadarReliabilityHandler) actor(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not authenticated")
		return 0, false
	}
	tenantID := subject.TenantID
	if tenantID <= 0 {
		tenantID = subject.UserID
	}
	if c.Request != nil {
		c.Request = c.Request.WithContext(middleware.WithRadarTenant(c.Request.Context(), tenantID))
	}
	return subject.UserID, true
}

func (h *RadarReliabilityHandler) require(c *gin.Context, permission service.RadarPermission) (int64, bool) {
	if h == nil || h.repo == nil {
		response.Error(c, http.StatusServiceUnavailable, "Radar reliability is not available")
		return 0, false
	}
	actorID, ok := h.actor(c)
	if !ok {
		return 0, false
	}
	if err := h.repo.Require(c.Request.Context(), actorID, permission); err != nil {
		if errors.Is(err, service.ErrRadarForbidden) {
			response.Forbidden(c, "Radar permission denied")
		} else {
			response.ErrorFrom(c, err)
		}
		return 0, false
	}
	return actorID, true
}

func (h *RadarReliabilityHandler) CreateLoadPlan(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionLoadPlanManage)
	if !ok {
		return
	}
	var input service.RadarLoadPlanInput
	if !decodeJSON(c, &input) {
		return
	}
	// Tenant ownership comes from the authenticated subject. A body supplied
	// tenant_id is ignored at this boundary.
	input.TenantID = actorID
	record, err := h.repo.CreateLoadPlan(c.Request.Context(), input, actorID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, record)
}

func (h *RadarReliabilityHandler) PublishLoadPlan(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionLoadPlanManage)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var record *service.RadarLoadPlanRecord
	var err error
	if scoped, ok := h.repo.(service.RadarTenantScopedReliabilityRepository); ok {
		record, err = scoped.PublishLoadPlanForTenant(c.Request.Context(), id, actorID, actorID)
	} else {
		record, err = h.repo.PublishLoadPlan(c.Request.Context(), id, actorID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "Load plan not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, record)
}

func (h *RadarReliabilityHandler) GetLoadPlan(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionView)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var record *service.RadarLoadPlanRecord
	var err error
	if scoped, ok := h.repo.(service.RadarTenantScopedReliabilityRepository); ok {
		record, err = scoped.GetLoadPlanForTenant(c.Request.Context(), id, actorID)
	} else {
		record, err = h.repo.GetLoadPlan(c.Request.Context(), id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "Load plan not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, record)
}
