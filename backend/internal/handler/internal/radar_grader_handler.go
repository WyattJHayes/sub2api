// Package internal contains the private worker-plane HTTP handlers for Radar.
package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const (
	defaultRadarWorkerLeaseTTL = 90 * time.Second
	maxRadarWorkerLeaseTTL     = 15 * time.Minute
)

type RadarGraderHandler struct {
	repo        service.EvaluationGradingRepository
	runner      service.EvaluationRunnerRepository
	reliability service.ReliabilitySnapshotPublisher
	execution   service.RadarReliabilityExecutionRepository
	config      *config.Config
}

func NewRadarGraderHandler(repo service.EvaluationGradingRepository, configs ...*config.Config) *RadarGraderHandler {
	var cfg *config.Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	h := &RadarGraderHandler{repo: repo, config: cfg}
	if runner, ok := repo.(service.EvaluationRunnerRepository); ok {
		h.runner = runner
	}
	if publisher, ok := repo.(service.ReliabilitySnapshotPublisher); ok {
		h.reliability = publisher
	}
	if execution, ok := repo.(service.RadarReliabilityExecutionRepository); ok {
		h.execution = execution
	}
	return h
}

func ProvideRadarGraderHandler(repo service.EvaluationGradingRepository, cfg *config.Config) *RadarGraderHandler {
	return NewRadarGraderHandler(repo, cfg)
}

// RegisterRadarGraderRoutes is kept beside the handler so internal callers can
// register the worker plane without depending on the server routes package.
func RegisterRadarGraderRoutes(r gin.IRouter, h *RadarGraderHandler) {
	if r == nil || h == nil {
		return
	}
	worker := r.Group("/internal/radar/v1")
	worker.POST("/grading-leases:claim", h.ClaimGradingLease)
	worker.POST("/leases:claim", h.ClaimAssignment)
	worker.POST("/leases/wait", h.WaitAssignment)
	worker.POST("/leases/:id/heartbeat", h.HeartbeatAssignment)
	worker.POST("/leases/:id/evidence", h.SubmitEvidence)
	worker.POST("/leases/:id/artifacts/presign", h.PresignArtifact)
	worker.POST("/leases/:id/artifacts/confirm", h.ConfirmArtifact)
	worker.POST("/grading-leases/:id/artifacts/:artifact_id/read", h.PresignGradingArtifactRead)
	worker.POST("/leases/:id/complete", h.CompleteAssignment)
	worker.POST("/leases/:id/fail", h.FailAssignment)
	worker.POST("/grading-leases/:id/heartbeat", h.HeartbeatGradingLease)
	worker.POST("/grading-leases/:id/complete", h.CompleteGradingLease)
	worker.POST("/grading-leases/:id/fail", h.FailGradingLease)
	worker.POST("/analysis-jobs:claim", h.ClaimAnalysisJob)
	worker.POST("/analysis-jobs/:id/complete", h.CompleteAnalysisJob)
	worker.POST("/analysis-jobs/:id/fail", h.FailAnalysisJob)
	worker.POST("/reliability-snapshots", h.PublishReliabilitySnapshot)
	worker.GET("/fault-experiments/:id", h.GetFaultExperiment)
	worker.POST("/fault-experiments/:id/actions", h.ApplyFaultAction)
	worker.POST("/fault-experiments/:id/events", h.AppendFaultEvent)
	worker.GET("/recovery-evidence/:id/observation", h.GetRecoveryObservation)
	worker.POST("/recovery-evidence/:id", h.PublishRecoveryEvidence)
}

type claimWorkerRequest struct {
	Capabilities []string `json:"capabilities"`
	GraderIDs    []string `json:"grader_ids"`
	LeaseSeconds int      `json:"lease_seconds"`
}

func (h *RadarGraderHandler) ClaimGradingLease(c *gin.Context) {
	if h == nil || h.repo == nil {
		response.InternalError(c, "grading repository unavailable")
		return
	}
	var request claimWorkerRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			response.BadRequest(c, "invalid grading lease request")
			return
		}
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "grader")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	if !h.touchWorkerHeartbeat(c, h.repo, workerID, "grader") {
		return
	}
	lease, err := h.repo.ClaimGradingLease(c.Request.Context(), workerID, mergeCapabilities(request.GraderIDs, request.Capabilities), radarLeaseTTL(request.LeaseSeconds))
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, lease)
}

type claimAssignmentRequest struct {
	Capabilities []string `json:"capabilities"`
	LeaseSeconds int      `json:"lease_seconds"`
}

func (h *RadarGraderHandler) ClaimAssignment(c *gin.Context) {
	if h == nil || h.runner == nil {
		response.InternalError(c, "runner repository unavailable")
		return
	}
	signer, err := h.evaluationContextSigner()
	if err != nil {
		response.InternalError(c, "radar evaluation signing is not configured")
		return
	}
	var req claimAssignmentRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "invalid assignment lease request")
			return
		}
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.runner.AuthenticateRunner(c.Request.Context(), token)
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	if !h.touchWorkerHeartbeat(c, h.runner, workerID, "runner") {
		return
	}
	lease, err := h.runner.ClaimAssignment(c.Request.Context(), workerID, req.Capabilities, radarLeaseTTL(req.LeaseSeconds))
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	if lease != nil {
		failClaim := func() {
			_ = h.runner.FailAssignment(
				c.Request.Context(), lease.ID, lease.Token,
				string(service.FailureClassInfrastructure), "evaluation_context_signing_failed",
				lease.LeaseEpoch,
			)
		}
		gatewayModelAlias, aliasErr := radarGatewayModelAlias(lease)
		if aliasErr != nil {
			failClaim()
			response.InternalError(c, "radar gateway model route is not configured")
			return
		}
		if strings.TrimSpace(lease.GatewayAPIKey) == "" {
			failClaim()
			response.InternalError(c, "radar evaluation API key is not configured")
			return
		}
		now := time.Now().UTC()
		gatewayToken, signerErr := signer.Sign(service.EvaluationContext{
			RunID: lease.RunID.String(), SampleID: lease.SampleID.String(),
			DatasetVersionID:      lease.DatasetVersionID.String(),
			DatasetKey:            lease.DatasetKey,
			DatasetVersion:        lease.DatasetVersion,
			DatasetManifestSHA256: lease.DatasetManifestSHA256,
			ExpectedModelAlias:    gatewayModelAlias,
			ExpectedRouteProfile:  h.config.Radar.RouteProfileVersion,
			APIKeyID:              lease.GatewayAPIKeyID, IssuedAt: now,
			ExpiresAt:    now.Add(time.Duration(h.config.Radar.MaxContextTTLSeconds) * time.Second),
			RouteTraceID: lease.RouteTraceID,
		})
		if signerErr != nil {
			failClaim()
			response.InternalError(c, "failed to sign radar evaluation context")
			return
		}
		lease.GatewayEvaluationToken = gatewayToken
	}
	response.Success(c, lease)
}

func (h *RadarGraderHandler) evaluationContextSigner() (*service.EvaluationContextSigner, error) {
	if h == nil || h.config == nil || !h.config.Radar.Enabled ||
		strings.TrimSpace(h.config.Radar.SigningSecret) == "" {
		return nil, errors.New("radar evaluation signing is not configured")
	}
	return service.NewEvaluationContextSigner(
		[]byte(h.config.Radar.SigningSecret),
		time.Duration(h.config.Radar.MaxContextTTLSeconds)*time.Second,
	)
}

func radarGatewayModelAlias(lease *service.AssignmentLease) (string, error) {
	if lease == nil {
		return "", errors.New("assignment lease is required")
	}
	var config map[string]any
	if err := json.Unmarshal(lease.ModelConfig, &config); err != nil {
		return "", err
	}
	for _, key := range []string{"route", "model_route", "model", "id"} {
		if route, ok := config[key].(string); ok && strings.TrimSpace(route) != "" {
			return strings.TrimSpace(route), nil
		}
	}
	return "", errors.New("gateway model route is required")
}

func (h *RadarGraderHandler) WaitAssignment(c *gin.Context) {
	if h == nil || h.runner == nil {
		response.InternalError(c, "runner repository unavailable")
		return
	}
	if _, ok := h.runnerAuth(c); !ok {
		return
	}
	response.Success(c, gin.H{"status": "waiting"})
}

func (h *RadarGraderHandler) HeartbeatAssignment(c *gin.Context) {
	id, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	runner, ok := h.runnerAuth(c)
	if !ok {
		return
	}
	var req struct {
		LeaseToken   string `json:"lease_token"`
		LeaseSeconds int    `json:"lease_seconds"`
		LeaseEpoch   int64  `json:"lease_epoch"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "invalid heartbeat request")
			return
		}
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(req.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	expires, err := runner.RenewAssignmentLease(c.Request.Context(), id, leaseToken, radarLeaseTTL(req.LeaseSeconds), req.LeaseEpoch)
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, gin.H{"assignment_id": id, "lease_expires_at": expires, "expires_at": expires})
}

func (h *RadarGraderHandler) SubmitEvidence(c *gin.Context) {
	id, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	runner, ok := h.runnerAuth(c)
	if !ok {
		return
	}
	var req struct {
		LeaseToken string          `json:"lease_token"`
		SampleID   uuid.UUID       `json:"sample_id"`
		Evidence   json.RawMessage `json:"evidence"`
		LeaseEpoch int64           `json:"lease_epoch"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid evidence submission")
		return
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(req.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	receipt, err := runner.SubmitEvidence(c.Request.Context(), service.EvidenceSubmission{AssignmentID: id, SampleID: req.SampleID, Evidence: req.Evidence, LeaseEpoch: req.LeaseEpoch}, leaseToken)
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, receipt)
}

func (h *RadarGraderHandler) PresignArtifact(c *gin.Context) {
	id, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	runner, ok := h.runnerAuth(c)
	if !ok {
		return
	}
	artifactRepo, ok := runner.(service.EvaluationArtifactRepository)
	if !ok {
		response.InternalError(c, "artifact repository unavailable")
		return
	}
	var request struct {
		LeaseToken string `json:"lease_token"`
		MIMEType   string `json:"mime_type"`
		Bytes      int64  `json:"bytes"`
		SHA256     string `json:"sha256"`
		LeaseEpoch int64  `json:"lease_epoch"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "invalid artifact presign request")
		return
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(request.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	upload, err := artifactRepo.PresignArtifact(c.Request.Context(), id, leaseToken, service.ArtifactPresignRequest{MIMEType: request.MIMEType, Bytes: request.Bytes, SHA256: request.SHA256, LeaseEpoch: request.LeaseEpoch})
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, upload)
}

func (h *RadarGraderHandler) ConfirmArtifact(c *gin.Context) {
	id, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	runner, ok := h.runnerAuth(c)
	if !ok {
		return
	}
	artifactRepo, ok := runner.(service.EvaluationArtifactRepository)
	if !ok {
		response.InternalError(c, "artifact repository unavailable")
		return
	}
	var request struct {
		LeaseToken string    `json:"lease_token"`
		ArtifactID uuid.UUID `json:"artifact_id"`
		ObjectKey  string    `json:"object_key"`
		SHA256     string    `json:"sha256"`
		Bytes      int64     `json:"bytes"`
		LeaseEpoch int64     `json:"lease_epoch"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "invalid artifact confirmation request")
		return
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(request.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	receipt, err := artifactRepo.ConfirmArtifact(c.Request.Context(), id, leaseToken, service.ArtifactConfirmation{ArtifactID: request.ArtifactID, ObjectKey: request.ObjectKey, SHA256: request.SHA256, Bytes: request.Bytes, LeaseEpoch: request.LeaseEpoch})
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, receipt)
}

func (h *RadarGraderHandler) PresignGradingArtifactRead(c *gin.Context) {
	leaseID, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	artifactID, err := uuid.Parse(strings.TrimSpace(c.Param("artifact_id")))
	if err != nil || artifactID == uuid.Nil {
		response.BadRequest(c, "invalid artifact id")
		return
	}
	reader, ok := h.repo.(service.EvaluationArtifactReadRepository)
	if !ok {
		response.InternalError(c, "artifact read repository unavailable")
		return
	}
	workerToken, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), workerToken, "grader")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	var request struct {
		LeaseToken string `json:"lease_token"`
		LeaseEpoch int64  `json:"lease_epoch"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			response.BadRequest(c, "invalid artifact read request")
			return
		}
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(request.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	download, err := reader.PresignGradingArtifactRead(c.Request.Context(), workerID, leaseID, leaseToken, artifactID, request.LeaseEpoch)
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, download)
}

func (h *RadarGraderHandler) CompleteAssignment(c *gin.Context) {
	id, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	runner, ok := h.runnerAuth(c)
	if !ok {
		return
	}
	var req struct {
		LeaseToken string `json:"lease_token"`
		LeaseEpoch int64  `json:"lease_epoch"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "invalid completion request")
			return
		}
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(req.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	if err := runner.CompleteAssignment(c.Request.Context(), id, leaseToken, req.LeaseEpoch); err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, gin.H{"assignment_id": id, "status": "completed"})
}

func (h *RadarGraderHandler) FailAssignment(c *gin.Context) {
	id, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	runner, ok := h.runnerAuth(c)
	if !ok {
		return
	}
	var req struct {
		LeaseToken   string `json:"lease_token"`
		FailureClass string `json:"failure_class"`
		FailureCode  string `json:"failure_code"`
		LeaseEpoch   int64  `json:"lease_epoch"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid failure request")
		return
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(req.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	if err := runner.FailAssignment(c.Request.Context(), id, leaseToken, req.FailureClass, req.FailureCode, req.LeaseEpoch); err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, gin.H{"assignment_id": id, "status": "failed"})
}

func (h *RadarGraderHandler) runnerAuth(c *gin.Context) (service.EvaluationRunnerRepository, bool) {
	if h == nil || h.runner == nil {
		response.InternalError(c, "runner repository unavailable")
		return nil, false
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return nil, false
	}
	workerID, err := h.runner.AuthenticateRunner(c.Request.Context(), token)
	if err != nil {
		h.writeWorkerError(c, err)
		return nil, false
	}
	bindRadarWorker(c, workerID)
	if !h.touchWorkerHeartbeat(c, h.runner, workerID, "runner") {
		return nil, false
	}
	return h.runner, true
}

func (h *RadarGraderHandler) touchWorkerHeartbeat(c *gin.Context, repo any, workerID uuid.UUID, workerKind string) bool {
	heartbeatRepo, ok := repo.(service.EvaluationWorkerHeartbeatRepository)
	if !ok {
		return true
	}
	if err := heartbeatRepo.TouchWorkerHeartbeat(c.Request.Context(), workerID, workerKind); err != nil {
		h.writeWorkerError(c, err)
		return false
	}
	return true
}

func (h *RadarGraderHandler) HeartbeatGradingLease(c *gin.Context) {
	leaseID, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "grader")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	var request struct {
		LeaseSeconds int    `json:"lease_seconds"`
		LeaseToken   string `json:"lease_token"`
		LeaseEpoch   int64  `json:"lease_epoch"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			response.BadRequest(c, "invalid heartbeat request")
			return
		}
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(request.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	expires, err := h.repo.HeartbeatGradingLease(c.Request.Context(), leaseID, leaseToken, radarLeaseTTL(request.LeaseSeconds), request.LeaseEpoch)
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, gin.H{"lease_id": leaseID, "expires_at": expires})
}

func (h *RadarGraderHandler) CompleteGradingLease(c *gin.Context) {
	leaseID, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "grader")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	var request struct {
		LeaseToken     string               `json:"lease_token"`
		Score          decimal.Decimal      `json:"score"`
		Passed         *bool                `json:"passed,omitempty"`
		FailureClass   service.FailureClass `json:"failure_class,omitempty"`
		FailureCode    string               `json:"failure_code,omitempty"`
		Explanation    string               `json:"explanation,omitempty"`
		EvidenceHashes []string             `json:"evidence_hashes"`
		LeaseEpoch     int64                `json:"lease_epoch"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "invalid score submission")
		return
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(request.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	score, err := h.repo.SubmitScore(c.Request.Context(), leaseID, leaseToken, service.ScoreSubmission{
		Score: request.Score, Passed: request.Passed, FailureClass: request.FailureClass,
		FailureCode: request.FailureCode, Explanation: request.Explanation,
		EvidenceHashes: request.EvidenceHashes, LeaseEpoch: request.LeaseEpoch,
	})
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, struct {
		ScoreRef    service.ScoreRef `json:"score_ref"`
		HeadVersion int              `json:"head_version"`
	}{
		ScoreRef: score.Ref, HeadVersion: score.HeadVersion,
	})
}

func (h *RadarGraderHandler) CompleteGrading(c *gin.Context) {
	h.CompleteGradingLease(c)
}

func (h *RadarGraderHandler) FailGradingLease(c *gin.Context) {
	leaseID, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "grader")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	var request struct {
		FailureClass string `json:"failure_class"`
		FailureCode  string `json:"failure_code"`
		LeaseToken   string `json:"lease_token"`
		LeaseEpoch   int64  `json:"lease_epoch"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "invalid grading failure request")
		return
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(request.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	if err := h.repo.FailGradingLease(c.Request.Context(), leaseID, leaseToken, request.FailureClass, request.FailureCode, request.LeaseEpoch); err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, gin.H{"lease_id": leaseID, "status": "failed"})
}

func (h *RadarGraderHandler) ClaimAnalysisJob(c *gin.Context) {
	if h == nil || h.repo == nil {
		response.InternalError(c, "grading repository unavailable")
		return
	}
	var request claimWorkerRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			response.BadRequest(c, "invalid analysis lease request")
			return
		}
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "statistics")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	if !h.touchWorkerHeartbeat(c, h.repo, workerID, "statistics") {
		return
	}
	lease, err := h.repo.ClaimAnalysisJob(c.Request.Context(), workerID, request.Capabilities, radarLeaseTTL(request.LeaseSeconds))
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, lease)
}

func (h *RadarGraderHandler) ClaimAnalysisLease(c *gin.Context) {
	h.ClaimAnalysisJob(c)
}

func (h *RadarGraderHandler) CompleteAnalysisJob(c *gin.Context) {
	jobID, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "statistics")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	var request struct {
		LeaseToken string `json:"lease_token"`
		service.AggregateSubmission
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "invalid aggregate submission")
		return
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(request.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	snapshot, err := h.repo.CompleteAnalysisJob(c.Request.Context(), jobID, leaseToken, request.AggregateSubmission)
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, snapshot)
}

func (h *RadarGraderHandler) CompleteAnalysis(c *gin.Context) {
	h.CompleteAnalysisJob(c)
}

func (h *RadarGraderHandler) FailAnalysisJob(c *gin.Context) {
	jobID, ok := parseRadarLeaseID(c)
	if !ok {
		return
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	if h == nil || h.repo == nil {
		response.InternalError(c, "grading repository unavailable")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "statistics")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	failureRepo, ok := h.repo.(service.AnalysisFailureRepository)
	if !ok {
		response.InternalError(c, "analysis failure repository unavailable")
		return
	}
	var request struct {
		LeaseToken  string `json:"lease_token"`
		FailureCode string `json:"failure_code"`
		LeaseEpoch  int64  `json:"lease_epoch"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "invalid analysis failure request")
		return
	}
	leaseToken := radarLeaseToken(c)
	if leaseToken == "" {
		leaseToken = strings.TrimSpace(request.LeaseToken)
	}
	if leaseToken == "" {
		response.BadRequest(c, "lease token is required")
		return
	}
	if err := failureRepo.FailAnalysisJob(c.Request.Context(), jobID, leaseToken, request.FailureCode, request.LeaseEpoch); err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, gin.H{"analysis_job_id": jobID, "status": "failed"})
}

// PublishReliabilitySnapshot accepts only statistics-worker submissions. The
// authenticated worker ID is injected after token validation, so callers
// cannot impersonate another worker in the payload.
func (h *RadarGraderHandler) PublishReliabilitySnapshot(c *gin.Context) {
	if h == nil || h.repo == nil || h.reliability == nil {
		response.InternalError(c, "reliability publisher unavailable")
		return
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "statistics")
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	bindRadarWorker(c, workerID)
	var submission service.ReliabilitySnapshotSubmission
	if err := c.ShouldBindJSON(&submission); err != nil {
		response.BadRequest(c, "invalid reliability snapshot submission")
		return
	}
	submission.WorkerID = workerID
	if strings.TrimSpace(submission.WorkerImageDigest) == "" {
		response.BadRequest(c, "worker image digest is required")
		return
	}
	receipt, err := h.reliability.PublishReliabilitySnapshot(c.Request.Context(), submission)
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, receipt)
}

func (h *RadarGraderHandler) controlPlaneWorkerAuth(c *gin.Context) bool {
	if h == nil || h.repo == nil || h.execution == nil {
		response.InternalError(c, "reliability execution repository unavailable")
		return false
	}
	token, ok := radarWorkerToken(c)
	if !ok {
		response.Unauthorized(c, "worker authorization required")
		return false
	}
	workerID, err := h.repo.AuthenticateWorker(c.Request.Context(), token, "statistics")
	if err != nil {
		h.writeWorkerError(c, err)
		return false
	}
	bindRadarWorker(c, workerID)
	return true
}

func bindRadarWorker(c *gin.Context, workerID uuid.UUID) {
	if c == nil || c.Request == nil || workerID == uuid.Nil {
		return
	}
	c.Request = c.Request.WithContext(service.WithRadarWorkerID(c.Request.Context(), workerID))
}

func parseRadarControlPlaneID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if err != nil || id == uuid.Nil {
		response.BadRequest(c, "invalid Radar control-plane id")
		return uuid.Nil, false
	}
	return id, true
}

func (h *RadarGraderHandler) GetFaultExperiment(c *gin.Context) {
	if !h.controlPlaneWorkerAuth(c) {
		return
	}
	id, ok := parseRadarControlPlaneID(c)
	if !ok {
		return
	}
	record, err := h.execution.GetApprovedFaultExperiment(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "approved fault experiment not found")
		return
	}
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, record)
}

func (h *RadarGraderHandler) ApplyFaultAction(c *gin.Context) {
	if !h.controlPlaneWorkerAuth(c) {
		return
	}
	id, ok := parseRadarControlPlaneID(c)
	if !ok {
		return
	}
	var request service.RadarFaultActionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "invalid fault action request")
		return
	}
	receipt, err := h.execution.ApplyFaultAction(c.Request.Context(), id, request)
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "fault experiment not found")
		return
	}
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, receipt)
}

func (h *RadarGraderHandler) AppendFaultEvent(c *gin.Context) {
	if !h.controlPlaneWorkerAuth(c) {
		return
	}
	id, ok := parseRadarControlPlaneID(c)
	if !ok {
		return
	}
	var event service.RadarFaultEventSubmission
	if err := c.ShouldBindJSON(&event); err != nil {
		response.BadRequest(c, "invalid fault event")
		return
	}
	if event.ExperimentID != uuid.Nil && event.ExperimentID != id {
		response.BadRequest(c, "fault event experiment does not match path")
		return
	}
	event.ExperimentID = id
	receipt, err := h.execution.AppendFaultEvent(c.Request.Context(), event)
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, receipt)
}

func (h *RadarGraderHandler) GetRecoveryObservation(c *gin.Context) {
	if !h.controlPlaneWorkerAuth(c) {
		return
	}
	id, ok := parseRadarControlPlaneID(c)
	if !ok {
		return
	}
	record, err := h.execution.GetRecoveryObservation(c.Request.Context(), id)
	if errors.Is(err, service.ErrRecoveryObservationNotFound) || errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "recovery observation not found")
		return
	}
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	var observation any
	if err := json.Unmarshal(record.Observation, &observation); err != nil {
		response.InternalError(c, "recovery observation is not valid JSON")
		return
	}
	response.Success(c, gin.H{"observation": observation})
}

func (h *RadarGraderHandler) PublishRecoveryEvidence(c *gin.Context) {
	if !h.controlPlaneWorkerAuth(c) {
		return
	}
	id, ok := parseRadarControlPlaneID(c)
	if !ok {
		return
	}
	var submission service.RadarRecoveryEvidenceSubmission
	if err := c.ShouldBindJSON(&submission); err != nil {
		response.BadRequest(c, "invalid recovery evidence")
		return
	}
	receipt, err := h.execution.PublishRecoveryEvidence(c.Request.Context(), id, submission)
	if errors.Is(err, service.ErrRecoveryObservationNotFound) {
		response.NotFound(c, "recovery observation not found")
		return
	}
	if err != nil {
		h.writeWorkerError(c, err)
		return
	}
	response.Success(c, receipt)
}

func (h *RadarGraderHandler) writeWorkerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.Header("Retry-After", "1")
		response.Error(c, http.StatusServiceUnavailable, "worker service temporarily unavailable")
	case errors.Is(err, service.ErrWorkerKindMismatch):
		response.Forbidden(c, "worker kind is not authorized")
	case errors.Is(err, service.ErrLeaseFenced), errors.Is(err, service.ErrGradingLeaseFenced), errors.Is(err, service.ErrAnalysisJobFenced):
		response.Error(c, http.StatusConflict, "worker lease fenced")
	case errors.Is(err, service.ErrRouteEvidenceNotSealed):
		c.Header("Retry-After", "1")
		response.Error(c, http.StatusServiceUnavailable, "route evidence is still sealing")
	case errors.Is(err, service.ErrEvidenceMismatch), errors.Is(err, service.ErrGraderIdentityMismatch),
		errors.Is(err, service.ErrAggregateRunMismatch), errors.Is(err, service.ErrAggregateInputMismatch),
		errors.Is(err, service.ErrScoreSubmissionInvalid):
		response.BadRequest(c, err.Error())
	case errors.Is(err, service.ErrArtifactInvalid), errors.Is(err, service.ErrArtifactNotFound), errors.Is(err, service.ErrArtifactObjectMismatch):
		response.BadRequest(c, err.Error())
	case errors.Is(err, service.ErrArtifactScanRejected):
		response.Error(c, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, service.ErrArtifactScanFailed), errors.Is(err, service.ErrArtifactScannerUnavailable),
		errors.Is(err, service.ErrArtifactObjectStoreUnavailable), errors.Is(err, service.ErrArtifactObjectMetadataUnavailable):
		c.Header("Retry-After", "1")
		response.Error(c, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, service.ErrReliabilitySnapshotInvalid):
		response.BadRequest(c, err.Error())
	case errors.Is(err, service.ErrFaultExperimentInvalid), errors.Is(err, service.ErrFaultActionInvalid),
		errors.Is(err, service.ErrFaultEventInvalid), errors.Is(err, service.ErrRecoveryEvidenceInvalid):
		response.BadRequest(c, err.Error())
	case errors.Is(err, service.ErrRecoveryObservationNotFound):
		response.NotFound(c, "recovery observation not found")
	case errors.Is(err, service.ErrWorkerIdentityMismatch):
		response.Forbidden(c, "worker identity is not authorized")
	case errors.Is(err, service.ErrRadarForbidden):
		response.Forbidden(c, "worker tenant is not authorized")
	default:
		response.InternalError(c, err.Error())
	}
}

func parseRadarLeaseID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if err != nil || id == uuid.Nil {
		response.BadRequest(c, "invalid worker lease id")
		return uuid.Nil, false
	}
	return id, true
}

func radarWorkerToken(c *gin.Context) (string, bool) {
	for _, header := range []string{"X-Radar-Worker-Token", "X-Worker-Token"} {
		if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
			return value, true
		}
	}
	if value := strings.TrimSpace(c.GetHeader("Authorization")); value != "" {
		parts := strings.SplitN(value, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && strings.TrimSpace(parts[1]) != "" {
			return strings.TrimSpace(parts[1]), true
		}
	}
	return "", false
}

func radarLeaseToken(c *gin.Context) string {
	for _, header := range []string{"X-Radar-Lease-Token", "X-Lease-Token"} {
		if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
			return value
		}
	}
	return ""
}

func radarLeaseTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultRadarWorkerLeaseTTL
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl > maxRadarWorkerLeaseTTL {
		return maxRadarWorkerLeaseTTL
	}
	return ttl
}

func mergeCapabilities(primary, secondary []string) []string {
	if len(primary) == 0 {
		return secondary
	}
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	merged := make([]string, 0, len(primary)+len(secondary))
	for _, values := range [][]string{primary, secondary} {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; !ok {
				seen[value] = struct{}{}
				merged = append(merged, value)
			}
		}
	}
	return merged
}
