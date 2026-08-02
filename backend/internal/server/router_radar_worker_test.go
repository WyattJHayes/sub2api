package server

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type radarWorkerRouteRegistrationStub struct{}

func (radarWorkerRouteRegistrationStub) ClaimAssignment(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) WaitAssignment(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) HeartbeatAssignment(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) SubmitEvidence(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) CompleteAssignment(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) FailAssignment(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) ClaimGradingLease(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) HeartbeatGradingLease(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) CompleteGradingLease(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) FailGradingLease(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) ClaimAnalysisJob(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) CompleteAnalysisJob(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) PublishReliabilitySnapshot(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) PresignArtifact(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) ConfirmArtifact(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) FailAnalysisJob(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) GetFaultExperiment(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) ApplyFaultAction(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) AppendFaultEvent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) GetRecoveryObservation(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
func (radarWorkerRouteRegistrationStub) PublishRecoveryEvidence(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func TestRegisterPrivateWorkerRoutesAddsRadarClaimEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{
		RadarWorker: radarWorkerRouteRegistrationStub{},
	}

	registerPrivateWorkerRoutes(router, handlers)

	registered := make(map[string]struct{}, len(router.Routes()))
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = struct{}{}
	}

	for _, endpoint := range []string{
		http.MethodPost + " /internal/radar/v1/leases:claim",
		http.MethodPost + " /internal/radar/v1/grading-leases:claim",
		http.MethodPost + " /internal/radar/v1/analysis-jobs:claim",
		http.MethodPost + " /internal/radar/v1/reliability-snapshots",
		http.MethodPost + " /internal/radar/v1/analysis-jobs/:id/fail",
		http.MethodGet + " /internal/radar/v1/fault-experiments/:id",
		http.MethodPost + " /internal/radar/v1/fault-experiments/:id/actions",
		http.MethodPost + " /internal/radar/v1/fault-experiments/:id/events",
		http.MethodGet + " /internal/radar/v1/recovery-evidence/:id/observation",
		http.MethodPost + " /internal/radar/v1/recovery-evidence/:id",
	} {
		_, ok := registered[endpoint]
		require.Truef(t, ok, "missing Radar worker endpoint %s", endpoint)
	}
}
