//go:build unit

package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type radarGraderHandlerRepoStub struct {
	kind                     string
	workerID                 uuid.UUID
	gradingLease             *service.GradingLease
	assignmentLease          *service.AssignmentLease
	score                    *service.Score
	analysisLease            *service.AnalysisJobLease
	analysisErr              error
	identifyErr              error
	claimErr                 error
	submitErr                error
	evidenceErr              error
	upload                   *service.ArtifactUpload
	receipt                  *service.ArtifactReceipt
	artifactErr              error
	claimCalls               int
	failedAssignmentID       uuid.UUID
	failedAssignmentToken    string
	failedAssignmentClass    string
	failedAssignmentCode     string
	completedAssignmentEpoch int64
}

func (s *radarGraderHandlerRepoStub) PresignArtifact(context.Context, uuid.UUID, string, service.ArtifactPresignRequest) (*service.ArtifactUpload, error) {
	return s.upload, s.artifactErr
}
func (s *radarGraderHandlerRepoStub) ConfirmArtifact(context.Context, uuid.UUID, string, service.ArtifactConfirmation) (*service.ArtifactReceipt, error) {
	return s.receipt, s.artifactErr
}
func (s *radarGraderHandlerRepoStub) AuthenticateRunner(context.Context, string) (uuid.UUID, error) {
	if s.identifyErr != nil {
		return uuid.Nil, s.identifyErr
	}
	return s.workerID, nil
}
func (s *radarGraderHandlerRepoStub) ClaimAssignment(context.Context, uuid.UUID, []string, time.Duration) (*service.AssignmentLease, error) {
	s.claimCalls++
	return s.assignmentLease, nil
}
func (s *radarGraderHandlerRepoStub) RenewAssignmentLease(context.Context, uuid.UUID, string, time.Duration, ...int64) (time.Time, error) {
	return time.Now().Add(time.Minute), nil
}
func (s *radarGraderHandlerRepoStub) SubmitEvidence(context.Context, service.EvidenceSubmission, string) (*service.EvidenceReceipt, error) {
	return nil, s.evidenceErr
}
func (s *radarGraderHandlerRepoStub) CompleteAssignment(_ context.Context, _ uuid.UUID, _ string, epoch ...int64) error {
	if len(epoch) > 0 {
		s.completedAssignmentEpoch = epoch[0]
	}
	return nil
}
func (s *radarGraderHandlerRepoStub) FailAssignment(_ context.Context, id uuid.UUID, token, failureClass, failureCode string, _ ...int64) error {
	s.failedAssignmentID = id
	s.failedAssignmentToken = token
	s.failedAssignmentClass = failureClass
	s.failedAssignmentCode = failureCode
	return nil
}

func (s *radarGraderHandlerRepoStub) AuthenticateWorker(context.Context, string, string) (uuid.UUID, error) {
	if s.identifyErr != nil {
		return uuid.Nil, s.identifyErr
	}
	return s.workerID, nil
}
func (s *radarGraderHandlerRepoStub) ClaimGradingLease(context.Context, uuid.UUID, []string, time.Duration) (*service.GradingLease, error) {
	return s.gradingLease, s.claimErr
}
func (s *radarGraderHandlerRepoStub) HeartbeatGradingLease(context.Context, uuid.UUID, string, time.Duration, ...int64) (time.Time, error) {
	return time.Now().Add(time.Minute), nil
}
func (s *radarGraderHandlerRepoStub) SubmitScore(context.Context, uuid.UUID, string, service.ScoreSubmission) (*service.Score, error) {
	return s.score, s.submitErr
}
func (s *radarGraderHandlerRepoStub) FailGradingLease(context.Context, uuid.UUID, string, string, string, ...int64) error {
	return nil
}
func (s *radarGraderHandlerRepoStub) ClaimAnalysisJob(context.Context, uuid.UUID, []string, time.Duration) (*service.AnalysisJobLease, error) {
	return s.analysisLease, nil
}
func (s *radarGraderHandlerRepoStub) CompleteAnalysisJob(context.Context, uuid.UUID, string, service.AggregateSubmission, ...int64) (*service.AggregateSnapshot, error) {
	return &service.AggregateSnapshot{ID: uuid.New()}, s.analysisErr
}

func TestRadarGraderHandlerRejectsRunnerTokenForGradingClaim(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &radarGraderHandlerRepoStub{identifyErr: service.ErrWorkerKindMismatch}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/grading-leases:claim", h.ClaimGradingLease)
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/grading-leases:claim", bytes.NewBufferString(`{"capabilities":["exact"]}`))
	req.Header.Set("Authorization", "Bearer runner-token")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusForbidden, resp.Code)
}

func TestRadarGraderHandlerSignsGatewayEvaluationTokenForAssignment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runID, sampleID := uuid.New(), uuid.New()
	datasetID := uuid.New()
	repo := &radarGraderHandlerRepoStub{
		workerID: uuid.New(),
		assignmentLease: &service.AssignmentLease{
			ID: uuid.New(), RunID: runID, SampleID: sampleID, ModelRoute: "model-alias",
			ModelConfig: json.RawMessage(`{"route":"gateway-model"}`),
			Attempt:     1, Token: "lease-token-123456", ExpiresAt: time.Now().Add(time.Minute),
			GatewayAPIKeyID: 42, GatewayAPIKey: "sk-radar",
			DatasetVersionID: datasetID, DatasetKey: "reasoning-smoke",
			DatasetVersion: "dataset-v1", DatasetManifestSHA256: strings.Repeat("d", 64),
			RouteTraceID: uuid.NewString(),
		},
	}
	secret := strings.Repeat("s", 32)
	cfg := &config.Config{Radar: config.RadarConfig{
		Enabled: true, SigningSecret: secret, MaxContextTTLSeconds: 300, RouteProfileVersion: "route-v42",
	}}
	h := NewRadarGraderHandler(repo, cfg)
	r := gin.New()
	r.POST("/internal/radar/v1/leases:claim", h.ClaimAssignment)
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/leases:claim", bytes.NewBufferString(`{"capabilities":["reasoning"]}`))
	req.Header.Set("Authorization", "Bearer runner-token")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	var envelope struct {
		Data *service.AssignmentLease `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Data)
	require.NotEmpty(t, envelope.Data.GatewayEvaluationToken)
	require.Equal(t, repo.assignmentLease.RouteTraceID, envelope.Data.RouteTraceID)
	signer, err := service.NewEvaluationContextSigner([]byte(secret), 5*time.Minute)
	require.NoError(t, err)
	claims, err := signer.Verify(envelope.Data.GatewayEvaluationToken, 42, time.Now())
	require.NoError(t, err)
	require.Equal(t, runID.String(), claims.RunID)
	require.Equal(t, sampleID.String(), claims.SampleID)
	require.Equal(t, datasetID.String(), claims.DatasetVersionID)
	require.Equal(t, "reasoning-smoke", claims.DatasetKey)
	require.Equal(t, "dataset-v1", claims.DatasetVersion)
	require.Equal(t, strings.Repeat("d", 64), claims.DatasetManifestSHA256)
	require.Equal(t, "gateway-model", claims.ExpectedModelAlias)
	require.Equal(t, repo.assignmentLease.RouteTraceID, claims.RouteTraceID)
}

func TestRadarGraderHandlerValidatesSigningBeforeClaim(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &radarGraderHandlerRepoStub{workerID: uuid.New()}
	h := NewRadarGraderHandler(repo, &config.Config{Radar: config.RadarConfig{Enabled: false}})
	r := gin.New()
	r.POST("/internal/radar/v1/leases:claim", h.ClaimAssignment)
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/leases:claim", bytes.NewBufferString(`{"capabilities":["reasoning"]}`))
	req.Header.Set("Authorization", "Bearer runner-token")
	resp := httptest.NewRecorder()

	r.ServeHTTP(resp, req)

	require.Equal(t, http.StatusInternalServerError, resp.Code)
	require.Zero(t, repo.claimCalls)
}

func TestRadarGraderHandlerFailsClaimedAssignmentWhenSigningInputIsInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	assignmentID := uuid.New()
	repo := &radarGraderHandlerRepoStub{
		workerID: uuid.New(),
		assignmentLease: &service.AssignmentLease{
			ID: assignmentID, RunID: uuid.New(), SampleID: uuid.New(),
			ModelConfig: json.RawMessage(`{}`), Token: "lease-token-123456",
			GatewayAPIKeyID: 42, GatewayAPIKey: "sk-radar",
			DatasetVersionID: uuid.New(), DatasetKey: "reasoning-smoke", DatasetVersion: "v1",
			DatasetManifestSHA256: strings.Repeat("d", 64), RouteTraceID: uuid.NewString(),
		},
	}
	cfg := &config.Config{Radar: config.RadarConfig{
		Enabled: true, SigningSecret: strings.Repeat("s", 32), MaxContextTTLSeconds: 300,
		RouteProfileVersion: "route-v42",
	}}
	h := NewRadarGraderHandler(repo, cfg)
	r := gin.New()
	r.POST("/internal/radar/v1/leases:claim", h.ClaimAssignment)
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/leases:claim", bytes.NewBufferString(`{"capabilities":["reasoning"]}`))
	req.Header.Set("Authorization", "Bearer runner-token")
	resp := httptest.NewRecorder()

	r.ServeHTTP(resp, req)

	require.Equal(t, http.StatusInternalServerError, resp.Code)
	require.Equal(t, assignmentID, repo.failedAssignmentID)
	require.Equal(t, "lease-token-123456", repo.failedAssignmentToken)
	require.Equal(t, string(service.FailureClassInfrastructure), repo.failedAssignmentClass)
	require.Equal(t, "evaluation_context_signing_failed", repo.failedAssignmentCode)
}

func TestRadarGraderHandlerForwardsAssignmentLeaseEpoch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &radarGraderHandlerRepoStub{workerID: uuid.New()}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/leases/:id/complete", h.CompleteAssignment)
	assignmentID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/leases/"+assignmentID.String()+"/complete", bytes.NewBufferString(`{"lease_token":"lease-token-123456","lease_epoch":7}`))
	req.Header.Set("Authorization", "Bearer runner-token")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, int64(7), repo.completedAssignmentEpoch)
}

func TestRadarGraderHandlerRetriesWhileRouteEvidenceIsUnsealed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &radarGraderHandlerRepoStub{
		workerID:    uuid.New(),
		evidenceErr: service.ErrRouteEvidenceNotSealed,
	}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/leases/:id/evidence", h.SubmitEvidence)
	assignmentID := uuid.New()
	sampleID := uuid.New()
	body := `{"lease_token":"lease-token-123456","lease_epoch":7,"sample_id":"` + sampleID.String() + `","evidence":{"route_trace_id":"trace-1"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/leases/"+assignmentID.String()+"/evidence", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer runner-token")
	resp := httptest.NewRecorder()

	r.ServeHTTP(resp, req)

	require.Equal(t, http.StatusServiceUnavailable, resp.Code)
	require.Equal(t, "1", resp.Header().Get("Retry-After"))
}

func TestRadarGraderHandlerReturnsOnlyScoreReceipt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scoreID := uuid.New()
	repo := &radarGraderHandlerRepoStub{
		workerID: uuid.New(),
		score:    &service.Score{ID: scoreID, SampleID: uuid.New(), Version: 1, Score: decimal.RequireFromString("0.75"), Ref: service.ScoreRef{ID: scoreID, CreatedAt: time.Now().UTC()}, HeadVersion: 1},
	}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/grading-leases/:id/complete", h.CompleteGradingLease)
	body := `{"score":"0.75","evidence_hashes":[]}`
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/grading-leases/"+uuid.NewString()+"/complete", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer grader-token")
	req.Header.Set("X-Radar-Lease-Token", "lease-token")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(envelope.Data, &fields))
	require.Len(t, fields, 2)
	var receipt struct {
		ScoreRef    service.ScoreRef `json:"score_ref"`
		HeadVersion int              `json:"head_version"`
	}
	require.NoError(t, json.Unmarshal(envelope.Data, &receipt))
	require.Equal(t, scoreID, receipt.ScoreRef.ID)
	require.Equal(t, 1, receipt.HeadVersion)
}

func TestRadarGraderHandlerPresignsArtifactForRunner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	artifactID := uuid.New()
	repo := &radarGraderHandlerRepoStub{workerID: uuid.New(), upload: &service.ArtifactUpload{ID: artifactID, ObjectKey: "runs/a.txt", UploadURL: "file:///tmp/runs/a.txt"}}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/leases/:id/artifacts/presign", h.PresignArtifact)
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/leases/"+uuid.NewString()+"/artifacts/presign", bytes.NewBufferString(`{"mime_type":"text/plain","bytes":4,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`))
	req.Header.Set("Authorization", "Bearer runner-token")
	req.Header.Set("X-Radar-Lease-Token", "lease-token")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
}

func TestRadarGraderHandlerConfirmsArtifact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	artifactID := uuid.New()
	repo := &radarGraderHandlerRepoStub{workerID: uuid.New(), receipt: &service.ArtifactReceipt{ID: artifactID, ObjectKey: "runs/a.txt", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Bytes: 4, MIMEType: "text/plain", ScanStatus: "clean"}}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/leases/:id/artifacts/confirm", h.ConfirmArtifact)
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/leases/"+uuid.NewString()+"/artifacts/confirm", bytes.NewBufferString(`{"artifact_id":"`+artifactID.String()+`","object_key":"runs/a.txt","sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","bytes":4}`))
	req.Header.Set("Authorization", "Bearer runner-token")
	req.Header.Set("X-Radar-Lease-Token", "lease-token")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
}

func TestRadarGraderHandlerReturnsAnalysisInputsOnClaim(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runID := uuid.New()
	analysisID := uuid.New()
	caseID := uuid.New()
	workerID := uuid.New()
	repo := &radarGraderHandlerRepoStub{
		workerID: workerID,
		analysisLease: &service.AnalysisJobLease{
			ID: analysisID, RunID: runID, CapabilityDomain: "reasoning", ModelRoute: "candidate:route-a",
			Window: "daily", AnalysisVersion: "v1", WindowStart: time.Now().UTC(),
			Token: "analysis-lease-token-123456", ExpiresAt: time.Now().Add(time.Minute),
			ScoreIDs: []uuid.UUID{uuid.New()},
			Pairs: []service.PairedScore{{
				CaseID: caseID, ModelRoute: "route-a", SampleIndex: 0,
				Weight:        decimal.RequireFromString("1"),
				BaselineScore: decimal.RequireFromString("0.90"), CandidateScore: decimal.RequireFromString("0.80"),
			}},
			History:         []service.AggregateHistoryPoint{{DeltaPP: decimal.RequireFromString("-2.5")}},
			InvalidFailures: []service.FailureClass{service.FailureClassUpstream},
		},
	}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/analysis-jobs:claim", h.ClaimAnalysisJob)
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/analysis-jobs:claim", bytes.NewBufferString(`{"capabilities":["reasoning"]}`))
	req.Header.Set("Authorization", "Bearer statistics-token")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	var envelope struct {
		Data *service.AnalysisJobLease `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Data)
	require.Len(t, envelope.Data.Pairs, 1)
	require.Equal(t, caseID, envelope.Data.Pairs[0].CaseID)
	require.Len(t, envelope.Data.History, 1)
	require.Equal(t, service.FailureClassUpstream, envelope.Data.InvalidFailures[0])
}

func TestRadarGraderHandlerRejectsMismatchedFrozenAggregateInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jobID := uuid.New()
	repo := &radarGraderHandlerRepoStub{
		workerID: uuid.New(), analysisErr: service.ErrAggregateInputMismatch,
	}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/analysis-jobs/:id/complete", h.CompleteAnalysisJob)
	req := httptest.NewRequest(http.MethodPost,
		"/internal/radar/v1/analysis-jobs/"+jobID.String()+"/complete",
		bytes.NewBufferString(`{"lease_token":"analysis-lease-token","input_set_hash":"`+strings.Repeat("a", 64)+`","aggregate":{}}`))
	req.Header.Set("Authorization", "Bearer statistics-token")
	resp := httptest.NewRecorder()

	r.ServeHTTP(resp, req)

	require.Equal(t, http.StatusBadRequest, resp.Code)
}

func TestRadarGraderHandlerReturnsGradingEvidenceContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	artifactID := uuid.New()
	assignmentID := uuid.New()
	repo := &radarGraderHandlerRepoStub{
		workerID: uuid.New(),
		gradingLease: &service.GradingLease{
			ID: uuid.New(), RunID: uuid.New(), SampleID: uuid.New(), AssignmentID: assignmentID,
			GraderID: "exact", GraderVersion: "v1", Attempt: 1, Token: "grader-lease-token-123456",
			ExpiresAt:        time.Now().Add(time.Minute),
			EvidenceManifest: json.RawMessage(`{"sample_id":"manifest"}`),
			Evidence:         []service.ArtifactReceipt{{ID: artifactID, ObjectKey: "runs/evidence.json", SHA256: strings.Repeat("a", 64), Bytes: 2, MIMEType: "application/json", ScanStatus: "clean"}},
			RouteTraceID:     "trace-grading",
			Case:             &service.EvaluationCaseSpec{CaseID: uuid.New(), CaseKey: "case-1", CapabilityDomain: "reasoning", GraderID: "exact", GraderVersion: "v1", ContentSHA256: strings.Repeat("b", 64), Confidentiality: "synthetic"},
		},
	}
	h := NewRadarGraderHandler(repo, &config.Config{})
	r := gin.New()
	r.POST("/internal/radar/v1/grading-leases:claim", h.ClaimGradingLease)
	req := httptest.NewRequest(http.MethodPost, "/internal/radar/v1/grading-leases:claim", bytes.NewBufferString(`{"capabilities":["exact"]}`))
	req.Header.Set("Authorization", "Bearer grader-token")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	var envelope struct {
		Data *service.GradingLease `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Data)
	require.JSONEq(t, `{"sample_id":"manifest"}`, string(envelope.Data.EvidenceManifest))
	require.Len(t, envelope.Data.Evidence, 1)
	require.Equal(t, artifactID, envelope.Data.Evidence[0].ID)
	require.Equal(t, "trace-grading", envelope.Data.RouteTraceID)
	require.Equal(t, "case-1", envelope.Data.Case.CaseKey)
}
