package handler

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type RadarHealthHandler struct {
	projection service.RadarProjectionRepository
	reports    service.QualityReportReader
}

func NewRadarHealthHandler(projection service.RadarProjectionRepository, reports service.QualityReportReader) *RadarHealthHandler {
	return &RadarHealthHandler{projection: projection, reports: reports}
}

type radarPublicModelHealth struct {
	ModelAlias        string                    `json:"model_alias"`
	HealthState       string                    `json:"health_state"`
	Freshness         *time.Time                `json:"freshness,omitempty"`
	P99MS             *float64                  `json:"p99_ms,omitempty"`
	ErrorRate         *float64                  `json:"error_rate,omitempty"`
	OverallConclusion service.QualityConclusion `json:"overall_conclusion"`
	AdulterationRisk  service.QualityConclusion `json:"adulteration_risk,omitempty"`
	DegradationRisk   service.QualityConclusion `json:"degradation_risk,omitempty"`
	CheckedAt         *time.Time                `json:"checked_at,omitempty"`
}

func (h *RadarHealthHandler) List(c *gin.Context) {
	if h == nil || h.projection == nil {
		response.Error(c, http.StatusServiceUnavailable, "Radar health is not available")
		return
	}
	subject, ok := servermiddleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	projections, err := h.projection.ListModelHealth(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	qualitySummaries := make(map[string]service.PublicQualitySummary)
	if h.reports != nil {
		summaries, err := h.reports.ListPublicQualitySummaries(service.WithRadarTenant(c.Request.Context(), subject.TenantID))
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		for _, summary := range summaries {
			if alias := qualityReportModelAlias(summary.ModelAlias); alias != "" {
				qualitySummaries[alias] = summary
			}
		}
	}

	byAlias := make(map[string]*radarPublicModelHealth, len(projections))
	for _, projection := range projections {
		alias := qualityReportModelAlias(projection.ModelRoute)
		if alias == "" {
			continue
		}
		item, exists := byAlias[alias]
		if !exists {
			item = &radarPublicModelHealth{
				ModelAlias:        alias,
				HealthState:       publicHealthState(projection.HealthState),
				OverallConclusion: service.QualityConclusionInsufficientCoverage,
			}
			byAlias[alias] = item
		} else {
			item.HealthState = moreSevereHealthState(item.HealthState, projection.HealthState)
		}
		mergePublicHealthMetrics(item, projection)
	}
	for alias, item := range byAlias {
		summary, ok := qualitySummaries[qualityReportModelAlias(alias)]
		if !ok {
			continue
		}
		item.OverallConclusion = summary.OverallConclusion
		item.AdulterationRisk = summary.AdulterationRisk
		item.DegradationRisk = summary.DegradationRisk
		if !summary.CheckedAt.IsZero() {
			value := summary.CheckedAt
			item.CheckedAt = &value
		}
		if !summary.FreshUntil.IsZero() {
			value := summary.FreshUntil
			item.Freshness = &value
		}
	}

	items := make([]radarPublicModelHealth, 0, len(byAlias))
	for _, item := range byAlias {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ModelAlias < items[j].ModelAlias })
	response.Success(c, items)
}

// Detail returns the public, tenant-scoped quality report for a tracked model.
func (h *RadarHealthHandler) Detail(c *gin.Context) {
	if h == nil || h.reports == nil {
		response.Error(c, http.StatusServiceUnavailable, "Quality report is not available")
		return
	}
	subject, ok := servermiddleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	modelAlias := qualityReportModelAlias(c.Param("alias"))
	report, err := h.reports.GetPublicQualityReport(
		service.WithRadarTenant(c.Request.Context(), subject.TenantID),
		modelAlias,
	)
	if errors.Is(err, service.ErrQualityReportNotFound) {
		response.NotFound(c, "Quality report not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, report)
}

func qualityReportModelAlias(alias string) string {
	return service.CanonicalModelRoute(strings.TrimSpace(alias))
}

func publicHealthState(value string) string {
	switch strings.TrimSpace(value) {
	case "healthy":
		return "healthy"
	case "degraded", "blocked":
		return "degraded"
	default:
		return "insufficient_evidence"
	}
}

func moreSevereHealthState(current, candidate string) string {
	current = publicHealthState(current)
	candidate = publicHealthState(candidate)
	rank := map[string]int{"insufficient_evidence": 0, "healthy": 1, "degraded": 2}
	if rank[candidate] > rank[current] {
		return candidate
	}
	return current
}

func mergePublicHealthMetrics(item *radarPublicModelHealth, projection service.RadarModelHealthProjection) {
	if projection.P99MS != nil && (item.P99MS == nil || *projection.P99MS > *item.P99MS) {
		value := *projection.P99MS
		item.P99MS = &value
	}
	if projection.ErrorRate != nil && (item.ErrorRate == nil || *projection.ErrorRate > *item.ErrorRate) {
		value := *projection.ErrorRate
		item.ErrorRate = &value
	}
	if !projection.Freshness.IsZero() && (item.Freshness == nil || projection.Freshness.After(*item.Freshness)) {
		value := projection.Freshness
		item.Freshness = &value
	}
}
