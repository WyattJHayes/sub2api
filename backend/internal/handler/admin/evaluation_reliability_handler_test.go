package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type radarReliabilityHandlerRepoStub struct {
	*service.StaticRadarAuthorizer
	createdInput  *service.RadarLoadPlanInput
	createdActor  int64
	createdTenant int64
	publishedID   uuid.UUID
}

func (s *radarReliabilityHandlerRepoStub) CreateLoadPlan(ctx context.Context, input service.RadarLoadPlanInput, actorID int64) (*service.RadarLoadPlanRecord, error) {
	s.createdInput = &input
	s.createdActor = actorID
	s.createdTenant, _ = middleware.RadarTenantID(ctx)
	return &service.RadarLoadPlanRecord{ID: uuid.New(), Status: "draft"}, nil
}

func (s *radarReliabilityHandlerRepoStub) PublishLoadPlan(_ context.Context, id uuid.UUID, _ int64) (*service.RadarLoadPlanRecord, error) {
	s.publishedID = id
	return &service.RadarLoadPlanRecord{ID: id, Status: "published"}, nil
}

func (s *radarReliabilityHandlerRepoStub) GetLoadPlan(_ context.Context, id uuid.UUID) (*service.RadarLoadPlanRecord, error) {
	return &service.RadarLoadPlanRecord{ID: id, Status: "published"}, nil
}

func radarReliabilityHandlerContext(userID int64, method string, body string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(method, "/", strings.NewReader(body))
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
	return c
}

func TestRadarReliabilityHandlerCreatesLoadPlanWithAuthenticatedActor(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarReliabilityHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarReliabilityHandler(repo)
	c := radarReliabilityHandlerContext(77, http.MethodPost, `{
		"tenant_id":7,"environment":"staging","route_profile_version":"route-v42",
		"model_aliases":["deepseek-chat"],"regions":["cn-east"],"traffic_mode":"closed_loop",
		"concurrency_levels":[1],"input_token_buckets":[128],"output_token_buckets":[64],
		"warmup_seconds":0,"measurement_seconds":60,"minimum_valid_requests":1,
		"max_run_cost":"1","max_concurrency":1,
		"client_image_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"generator_version":"loadgen-v1"
	}`)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 77})
	h.CreateLoadPlan(c)

	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.NotNil(t, repo.createdInput)
	require.Equal(t, int64(77), repo.createdActor)
	require.Equal(t, int64(77), repo.createdTenant)
	require.Equal(t, int64(77), repo.createdInput.TenantID)
	require.Equal(t, decimal.RequireFromString("1"), repo.createdInput.MaxRunCost)
}

func TestRadarReliabilityHandlerRejectsViewerMutation(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleViewer}})
	repo := &radarReliabilityHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarReliabilityHandler(repo)
	c := radarReliabilityHandlerContext(77, http.MethodPost, "")
	h.CreateLoadPlan(c)
	require.Equal(t, http.StatusForbidden, c.Writer.Status())
	require.Nil(t, repo.createdInput)
}

func TestRadarReliabilityHandlerPublishesLoadPlanByPathID(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarReliabilityHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarReliabilityHandler(repo)
	c := radarReliabilityHandlerContext(77, http.MethodPost, "")
	c.Params = gin.Params{{Key: "id", Value: uuid.NewString()}}
	h.PublishLoadPlan(c)
	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.Equal(t, c.Params[0].Value, repo.publishedID.String())
}
