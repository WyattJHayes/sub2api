package routes

import "github.com/gin-gonic/gin"

type radarWorkerHTTPHandler interface {
	ClaimAssignment(*gin.Context)
	WaitAssignment(*gin.Context)
	HeartbeatAssignment(*gin.Context)
	SubmitEvidence(*gin.Context)
	CompleteAssignment(*gin.Context)
	FailAssignment(*gin.Context)
	ClaimGradingLease(*gin.Context)
	HeartbeatGradingLease(*gin.Context)
	CompleteGradingLease(*gin.Context)
	FailGradingLease(*gin.Context)
	ClaimAnalysisJob(*gin.Context)
	CompleteAnalysisJob(*gin.Context)
}

// RegisterRadarWorkerRoutes registers the private worker plane. Authentication
// and worker-kind fencing happen in the handler after the route is matched.
// The caller may pass either the engine or a router group rooted at a trusted
// internal ingress.
func RegisterRadarWorkerRoutes(r gin.IRouter, h radarWorkerHTTPHandler) {
	if r == nil || h == nil {
		return
	}
	worker := r.Group("/internal/radar/v1")
	worker.POST("/grading-leases:claim", h.ClaimGradingLease)
	worker.POST("/leases:claim", h.ClaimAssignment)
	worker.POST("/leases/wait", h.WaitAssignment)
	worker.POST("/leases/:id/heartbeat", h.HeartbeatAssignment)
	worker.POST("/leases/:id/evidence", h.SubmitEvidence)
	if artifactHandler, ok := h.(interface {
		PresignArtifact(*gin.Context)
		ConfirmArtifact(*gin.Context)
	}); ok {
		worker.POST("/leases/:id/artifacts/presign", artifactHandler.PresignArtifact)
		worker.POST("/leases/:id/artifacts/confirm", artifactHandler.ConfirmArtifact)
	}
	worker.POST("/leases/:id/complete", h.CompleteAssignment)
	worker.POST("/leases/:id/fail", h.FailAssignment)
	worker.POST("/grading-leases/:id/heartbeat", h.HeartbeatGradingLease)
	worker.POST("/grading-leases/:id/complete", h.CompleteGradingLease)
	worker.POST("/grading-leases/:id/fail", h.FailGradingLease)
	worker.POST("/analysis-jobs:claim", h.ClaimAnalysisJob)
	worker.POST("/analysis-jobs/:id/complete", h.CompleteAnalysisJob)
}
