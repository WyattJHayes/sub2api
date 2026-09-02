package routes

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type radarWorkerRouteStub struct{}

type radarWorkerArtifactRouteStub struct {
	radarWorkerRouteStub
}

type radarWorkerGradingArtifactReadRouteStub struct {
	radarWorkerRouteStub
}

type radarWorkerAllArtifactRouteStub struct {
	radarWorkerArtifactRouteStub
}

func (radarWorkerRouteStub) ClaimAssignment(c *gin.Context)     { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) WaitAssignment(c *gin.Context)      { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) HeartbeatAssignment(c *gin.Context) { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) SubmitEvidence(c *gin.Context)      { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) CompleteAssignment(c *gin.Context)  { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) FailAssignment(c *gin.Context)      { c.Status(http.StatusNoContent) }

func (radarWorkerRouteStub) ClaimGradingLease(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerRouteStub) HeartbeatGradingLease(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerRouteStub) CompleteGradingLease(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerRouteStub) FailGradingLease(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerRouteStub) ClaimAnalysisJob(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerRouteStub) CompleteAnalysisJob(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerRouteStub) PublishReliabilitySnapshot(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerArtifactRouteStub) PresignArtifact(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerArtifactRouteStub) ConfirmArtifact(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerGradingArtifactReadRouteStub) PresignGradingArtifactRead(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerAllArtifactRouteStub) PresignGradingArtifactRead(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func (radarWorkerRouteStub) FailAnalysisJob(c *gin.Context)         { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) GetFaultExperiment(c *gin.Context)      { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) ApplyFaultAction(c *gin.Context)        { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) AppendFaultEvent(c *gin.Context)        { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) GetRecoveryObservation(c *gin.Context)  { c.Status(http.StatusNoContent) }
func (radarWorkerRouteStub) PublishRecoveryEvidence(c *gin.Context) { c.Status(http.StatusNoContent) }

func TestRegisterRadarWorkerRoutesRegistersPrivateWorkerEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRadarWorkerRoutes(router, radarWorkerAllArtifactRouteStub{})

	registered := make(map[string]struct{}, len(router.Routes()))
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = struct{}{}
	}

	for _, endpoint := range []string{
		http.MethodPost + " /internal/radar/v1/leases:claim",
		http.MethodPost + " /internal/radar/v1/leases/wait",
		http.MethodPost + " /internal/radar/v1/leases/:id/heartbeat",
		http.MethodPost + " /internal/radar/v1/leases/:id/evidence",
		http.MethodPost + " /internal/radar/v1/leases/:id/artifacts/presign",
		http.MethodPost + " /internal/radar/v1/leases/:id/artifacts/confirm",
		http.MethodPost + " /internal/radar/v1/leases/:id/complete",
		http.MethodPost + " /internal/radar/v1/leases/:id/fail",
		http.MethodPost + " /internal/radar/v1/grading-leases:claim",
		http.MethodPost + " /internal/radar/v1/grading-leases/:id/heartbeat",
		http.MethodPost + " /internal/radar/v1/grading-leases/:id/complete",
		http.MethodPost + " /internal/radar/v1/grading-leases/:id/fail",
		http.MethodPost + " /internal/radar/v1/grading-leases/:id/artifacts/:artifact_id/read",
		http.MethodPost + " /internal/radar/v1/analysis-jobs:claim",
		http.MethodPost + " /internal/radar/v1/analysis-jobs/:id/complete",
		http.MethodPost + " /internal/radar/v1/analysis-jobs/:id/fail",
		http.MethodPost + " /internal/radar/v1/reliability-snapshots",
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

func TestRegisterRadarWorkerRoutesRegistersArtifactUploadWithoutGraderRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRadarWorkerRoutes(router, radarWorkerArtifactRouteStub{})

	registered := make(map[string]struct{}, len(router.Routes()))
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = struct{}{}
	}

	for _, endpoint := range []string{
		http.MethodPost + " /internal/radar/v1/leases/:id/artifacts/presign",
		http.MethodPost + " /internal/radar/v1/leases/:id/artifacts/confirm",
	} {
		_, ok := registered[endpoint]
		require.Truef(t, ok, "missing Radar artifact endpoint %s", endpoint)
	}
}

func TestRegisterRadarWorkerRoutesRegistersGraderReadWithoutArtifactUpload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRadarWorkerRoutes(router, radarWorkerGradingArtifactReadRouteStub{})

	registered := make(map[string]struct{}, len(router.Routes()))
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = struct{}{}
	}

	endpoint := http.MethodPost + " /internal/radar/v1/grading-leases/:id/artifacts/:artifact_id/read"
	_, ok := registered[endpoint]
	require.Truef(t, ok, "missing Radar grading artifact endpoint %s", endpoint)
}
