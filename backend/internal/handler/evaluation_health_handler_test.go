package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type radarHealthProjectionStub struct {
	models       []service.RadarModelHealthProjection
	tenantID     int64
	tenantScoped bool
}

type radarHealthReportStub struct {
	summaries      []service.PublicQualitySummary
	report         *service.PublicQualityReport
	reportErr      error
	listTenantID   int64
	detailTenantID int64
	detailAlias    string
}

func (s *radarHealthReportStub) ListPublicQualitySummaries(ctx context.Context) ([]service.PublicQualitySummary, error) {
	s.listTenantID, _ = service.RadarTenantID(ctx)
	return s.summaries, nil
}

func (s *radarHealthReportStub) GetPublicQualityReport(ctx context.Context, modelAlias string) (*service.PublicQualityReport, error) {
	s.detailTenantID, _ = service.RadarTenantID(ctx)
	s.detailAlias = modelAlias
	return s.report, s.reportErr
}

func (s *radarHealthProjectionStub) ListModelHealth(ctx context.Context) ([]service.RadarModelHealthProjection, error) {
	s.tenantID, s.tenantScoped = service.RadarTenantID(ctx)
	return s.models, nil
}
func (*radarHealthProjectionStub) ListRuns(context.Context) ([]service.RadarRunProjection, error) {
	return nil, nil
}
func (*radarHealthProjectionStub) ListAlerts(context.Context) ([]service.RadarAlertProjection, error) {
	return nil, nil
}
func (*radarHealthProjectionStub) ListGates(context.Context) ([]service.RadarGateProjection, error) {
	return nil, nil
}
func (*radarHealthProjectionStub) ListWorkers(context.Context) ([]service.RadarWorkerProjection, error) {
	return nil, nil
}
func (*radarHealthProjectionStub) ListDatasets(context.Context) ([]service.RadarDatasetProjection, error) {
	return nil, nil
}

func TestRadarHealthReturnsGlobalSanitizedModelProjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	freshness := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	baseline := 0.87
	candidate := 0.71
	delta := -16.0
	ciLow := -22.0
	ciHigh := -10.0
	sampleCount := 42
	p99 := 850.0
	errorRate := 0.01
	projection := &radarHealthProjectionStub{models: []service.RadarModelHealthProjection{{
		ModelRoute: "gpt-5.6-sol", CapabilityDomain: "reasoning", HealthState: "degraded", Freshness: freshness,
		BaselineScore: &baseline, CandidateScore: &candidate, DeltaPP: &delta, CILowPP: &ciLow, CIHighPP: &ciHigh,
		SampleCount: &sampleCount, P99MS: &p99, ErrorRate: &errorRate,
	}}}
	h := NewRadarHealthHandler(projection, nil)
	router := gin.New()
	router.GET("/api/v1/radar/health", func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 77, TenantID: 17})
		h.List(c)
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/radar/health", nil))

	require.Equal(t, http.StatusOK, response.Code)
	require.False(t, projection.tenantScoped)
	require.Zero(t, projection.tenantID)
	var payload struct {
		Data []struct {
			ModelAlias  string     `json:"model_alias"`
			HealthState string     `json:"health_state"`
			Freshness   *time.Time `json:"freshness"`
			P99MS       *float64   `json:"p99_ms"`
			ErrorRate   *float64   `json:"error_rate"`
		}
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Len(t, payload.Data, 1)
	require.Equal(t, "gpt-5.6-sol", payload.Data[0].ModelAlias)
	require.Equal(t, "degraded", payload.Data[0].HealthState)
	require.Equal(t, freshness, *payload.Data[0].Freshness)
	require.Equal(t, p99, *payload.Data[0].P99MS)
	require.Equal(t, errorRate, *payload.Data[0].ErrorRate)

	body := strings.ToLower(response.Body.String())
	for _, forbidden := range []string{
		"account_id", "channel_id", "prompt", "completion", "evidence_hash",
		"credential", "policy", "route_profile", "cost", "public_domain_scores",
		"capability_domain", "baseline_score", "candidate_score", "delta_pp",
		"ci_low_pp", "ci_high_pp", "sample_count",
	} {
		require.NotContains(t, body, forbidden)
	}
}

func TestRadarHealthListEnrichesGlobalProjectionWithTenantQualitySummary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checkedAt := time.Date(2026, time.August, 11, 1, 0, 0, 0, time.UTC)
	projection := &radarHealthProjectionStub{models: []service.RadarModelHealthProjection{{
		ModelRoute: "model-a", HealthState: "healthy",
	}}}
	reports := &radarHealthReportStub{summaries: []service.PublicQualitySummary{{
		ModelAlias: "model-a", OverallConclusion: service.QualityConclusionSuspected,
		AdulterationRisk: service.QualityConclusionHighRisk, DegradationRisk: service.QualityConclusionObserve,
		CheckedAt: checkedAt, FreshUntil: checkedAt.Add(time.Hour),
	}}}
	h := NewRadarHealthHandler(projection, reports)
	router := gin.New()
	router.GET("/api/v1/radar/health", func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 77, TenantID: 17})
		h.List(c)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/radar/health", nil))

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, int64(17), reports.listTenantID)
	var payload struct {
		Data []struct {
			OverallConclusion service.QualityConclusion `json:"overall_conclusion"`
			AdulterationRisk  service.QualityConclusion `json:"adulteration_risk"`
			DegradationRisk   service.QualityConclusion `json:"degradation_risk"`
			CheckedAt         time.Time                 `json:"checked_at"`
		}
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Len(t, payload.Data, 1)
	require.Equal(t, service.QualityConclusionSuspected, payload.Data[0].OverallConclusion)
	require.Equal(t, service.QualityConclusionHighRisk, payload.Data[0].AdulterationRisk)
	require.Equal(t, service.QualityConclusionObserve, payload.Data[0].DegradationRisk)
	require.Equal(t, checkedAt, payload.Data[0].CheckedAt)
}

func TestRadarHealthListEnrichesPrefixedProjectionWithCanonicalQualitySummary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	projection := &radarHealthProjectionStub{models: []service.RadarModelHealthProjection{
		{ModelRoute: "candidate:route-a", HealthState: "healthy"},
		{ModelRoute: "route-a", HealthState: "degraded"},
	}}
	reports := &radarHealthReportStub{summaries: []service.PublicQualitySummary{{
		ModelAlias: "route-a", OverallConclusion: service.QualityConclusionSuspected,
		AdulterationRisk: service.QualityConclusionHighRisk, DegradationRisk: service.QualityConclusionObserve,
	}}}
	h := NewRadarHealthHandler(projection, reports)
	router := gin.New()
	router.GET("/api/v1/radar/health", func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 77, TenantID: 17})
		h.List(c)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/radar/health", nil))

	require.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Data []struct {
			ModelAlias        string                    `json:"model_alias"`
			OverallConclusion service.QualityConclusion `json:"overall_conclusion"`
		}
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Len(t, payload.Data, 1)
	require.Equal(t, "route-a", payload.Data[0].ModelAlias)
	require.Equal(t, service.QualityConclusionSuspected, payload.Data[0].OverallConclusion)
}

func TestRadarHealthDetailReturnsTenantScopedPublicReport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reports := &radarHealthReportStub{report: &service.PublicQualityReport{
		ModelAlias: "model-a", OverallConclusion: service.QualityConclusionObserve,
		SourceAttribution: service.QualitySourceAttribution{
			State:        service.QualitySourceInsufficientEvidence,
			EvidenceCode: service.QualityEvidenceSourceInsufficientEvidence,
		},
	}}
	h := NewRadarHealthHandler(&radarHealthProjectionStub{}, reports)
	router := gin.New()
	router.GET("/api/v1/radar/models/:alias/quality-report", func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 77, TenantID: 17})
		h.Detail(c)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/radar/models/model-a/quality-report", nil))

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "model-a", reports.detailAlias)
	require.Equal(t, int64(17), reports.detailTenantID)
	require.NotContains(t, response.Body.String(), "route_trace_id")
	require.NotContains(t, response.Body.String(), "probe_spec_hash")
}

func TestRadarHealthDetailLooksUpPrefixedAliasAsCanonicalQualityAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reports := &radarHealthReportStub{report: &service.PublicQualityReport{ModelAlias: "route-a"}}
	h := NewRadarHealthHandler(&radarHealthProjectionStub{}, reports)
	router := gin.New()
	router.GET("/api/v1/radar/models/:alias/quality-report", func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 77, TenantID: 17})
		h.Detail(c)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/radar/models/candidate:route-a/quality-report", nil))

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "route-a", reports.detailAlias)
}

func TestRadarHealthDetailReturnsNotFoundForTenantScopedMiss(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reports := &radarHealthReportStub{reportErr: service.ErrQualityReportNotFound}
	h := NewRadarHealthHandler(&radarHealthProjectionStub{}, reports)
	router := gin.New()
	router.GET("/api/v1/radar/models/:alias/quality-report", func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 77, TenantID: 18})
		h.Detail(c)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/radar/models/model-a/quality-report", nil))

	require.Equal(t, http.StatusNotFound, response.Code)
}
