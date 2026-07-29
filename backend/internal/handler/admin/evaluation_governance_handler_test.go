package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type radarGovernanceHandlerRepoStub struct {
	*service.StaticRadarAuthorizer
	proposed             *service.RadarBaselineInput
	gateDecision         *service.RadarGateDecisionInput
	releaseSubject       *service.ReleaseSubjectInput
	policyActivation     *service.RadarGatePolicyActivationInput
	baselineActivation   *service.RadarBaselineActivationInput
	dataset              *service.CreateRadarDatasetInput
	plan                 *service.CreateRadarPlanInput
	run                  *service.CreateRunInput
	evaluationKeyID      int64
	evaluationKeyActorID int64
}

type radarRunControlHandlerRepoStub struct{ radarGovernanceHandlerRepoStub }

type radarRevisionBatchHandlerRepoStub struct {
	radarGovernanceHandlerRepoStub
	created            *service.CreateRevisionBatchInput
	control            *service.RevisionBatchControlInput
	controlAction      string
	compensating       *service.CompensatingScoreHeadInput
	compensatingResult service.CompensatingScoreHeadResult
}

func (s *radarRevisionBatchHandlerRepoStub) CreateRevisionBatch(_ context.Context, input service.CreateRevisionBatchInput) (*service.RevisionBatch, error) {
	s.created = &input
	return &service.RevisionBatch{ID: uuid.New(), RunID: input.RunID, Status: service.RevisionBatchRunning}, nil
}
func (s *radarRevisionBatchHandlerRepoStub) revisionControl(action string, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	s.controlAction = action
	s.control = &input
	return &service.RevisionBatch{ID: input.BatchID, Status: service.RevisionBatchRunning, ControlEpoch: 2}, nil
}
func (s *radarRevisionBatchHandlerRepoStub) FenceRevisionBatch(_ context.Context, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	return s.revisionControl("fence", input)
}
func (s *radarRevisionBatchHandlerRepoStub) ResumeRevisionBatch(_ context.Context, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	return s.revisionControl("resume", input)
}
func (s *radarRevisionBatchHandlerRepoStub) CancelRevisionBatch(_ context.Context, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	return s.revisionControl("cancel", input)
}
func (s *radarRevisionBatchHandlerRepoStub) RepairRevisionBatch(_ context.Context, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	return s.revisionControl("repair", input)
}
func (s *radarRevisionBatchHandlerRepoStub) ApproveCompensatingScoreHead(_ context.Context, input service.CompensatingScoreHeadInput) (*service.CompensatingScoreHeadResult, error) {
	s.compensating = &input
	result := s.compensatingResult
	result.BatchID = input.BatchID
	result.ScoreRef = input.ScoreRef
	return &result, nil
}

func (s *radarRunControlHandlerRepoStub) PauseRun(_ context.Context, runID uuid.UUID, _ string, _ int64, _ string) (*service.RunControlResult, error) {
	return &service.RunControlResult{RunID: runID, FromStatus: "running", ToStatus: "paused"}, nil
}
func (s *radarRunControlHandlerRepoStub) ResumeRun(context.Context, uuid.UUID, string, int64, string) (*service.RunControlResult, error) {
	return &service.RunControlResult{FromStatus: "paused", ToStatus: "running"}, nil
}
func (s *radarRunControlHandlerRepoStub) CancelRun(context.Context, uuid.UUID, string, int64, string) (*service.RunControlResult, error) {
	return &service.RunControlResult{ToStatus: "cancelled"}, nil
}
func (s *radarRunControlHandlerRepoStub) FenceRun(context.Context, uuid.UUID, string, int64, string) (*service.RunControlResult, error) {
	return &service.RunControlResult{ToStatus: "running", CurrentEpoch: 1}, nil
}

func (s *radarGovernanceHandlerRepoStub) RegisterRadarWorker(context.Context, service.RadarWorkerRegistrationInput, int64, string) (*service.RadarWorkerRecord, error) {
	return &service.RadarWorkerRecord{ID: uuid.New(), TokenFingerprint: "abcd1234efgh", ClaimMode: service.WorkerClaimsOpen}, nil
}
func (s *radarGovernanceHandlerRepoStub) RotateRadarWorkerToken(context.Context, uuid.UUID, string, int64, string) (*service.RadarWorkerRecord, error) {
	return &service.RadarWorkerRecord{ID: uuid.New(), TokenFingerprint: "abcd1234efgh", TokenEpoch: 1}, nil
}
func (s *radarGovernanceHandlerRepoStub) SetRadarWorkerClaimMode(context.Context, uuid.UUID, service.WorkerClaimMode, int64, string) (*service.RadarWorkerRecord, error) {
	return &service.RadarWorkerRecord{ID: uuid.New(), ClaimMode: service.WorkerClaimsPaused}, nil
}
func (s *radarGovernanceHandlerRepoStub) DisableRadarWorker(context.Context, uuid.UUID, int64, string) (*service.RadarWorkerRecord, error) {
	return &service.RadarWorkerRecord{ID: uuid.New(), Status: "disabled", TokenFingerprint: "abcd1234efgh"}, nil
}

func (s *radarGovernanceHandlerRepoStub) EnableEvaluationKey(_ context.Context, keyID, actorID int64) (*service.RadarEvaluationKeyRecord, error) {
	s.evaluationKeyID = keyID
	s.evaluationKeyActorID = actorID
	return &service.RadarEvaluationKeyRecord{ID: keyID, IsEvaluation: true}, nil
}

func (s *radarGovernanceHandlerRepoStub) CreateDataset(_ context.Context, input service.CreateRadarDatasetInput) (*service.RadarDatasetRecord, error) {
	s.dataset = &input
	return &service.RadarDatasetRecord{ID: uuid.New(), DatasetKey: input.DatasetKey, Version: input.Version, Status: service.DatasetStatusDraft}, nil
}
func (s *radarGovernanceHandlerRepoStub) PublishDataset(context.Context, uuid.UUID, int64) (*service.RadarDatasetRecord, error) {
	return &service.RadarDatasetRecord{ID: uuid.New(), Status: service.DatasetStatusPublished}, nil
}
func (s *radarGovernanceHandlerRepoStub) CreatePlan(_ context.Context, input service.CreateRadarPlanInput) (*service.RadarPlanRecord, error) {
	s.plan = &input
	return &service.RadarPlanRecord{ID: uuid.New(), Name: input.Name}, nil
}
func (s *radarGovernanceHandlerRepoStub) CreateRunWithMatrix(_ context.Context, input service.CreateRunInput) (*service.EvaluationRun, error) {
	s.run = &input
	return &service.EvaluationRun{ID: uuid.New(), PlanID: input.PlanID}, nil
}

func (s *radarGovernanceHandlerRepoStub) CreateRoleBinding(context.Context, service.RadarRoleBindingInput) (*service.RadarRoleBinding, error) {
	return &service.RadarRoleBinding{ID: uuid.New(), Enabled: true}, nil
}
func (s *radarGovernanceHandlerRepoStub) DisableRoleBinding(context.Context, uuid.UUID, int64) error {
	return nil
}
func (s *radarGovernanceHandlerRepoStub) ListRoleBindings(context.Context, *int64) ([]service.RadarRoleBinding, error) {
	return []service.RadarRoleBinding{}, nil
}
func (s *radarGovernanceHandlerRepoStub) ProposeBaseline(_ context.Context, input service.RadarBaselineInput) (*service.RadarBaseline, error) {
	s.proposed = &input
	return &service.RadarBaseline{ID: uuid.New(), ProposedBy: input.ProposedBy}, nil
}
func (s *radarGovernanceHandlerRepoStub) ApproveBaseline(context.Context, service.RadarBaselineApprovalInput) (*service.RadarBaselineApproval, error) {
	return &service.RadarBaselineApproval{}, nil
}
func (s *radarGovernanceHandlerRepoStub) ActivateBaseline(context.Context, uuid.UUID, int64) (*service.RadarBaseline, error) {
	return &service.RadarBaseline{}, nil
}
func (s *radarGovernanceHandlerRepoStub) GetBaseline(context.Context, uuid.UUID) (*service.RadarBaseline, error) {
	return &service.RadarBaseline{}, nil
}
func (s *radarGovernanceHandlerRepoStub) CreateGatePolicy(context.Context, service.RadarGatePolicyInput) (*service.RadarGatePolicyRecord, error) {
	return &service.RadarGatePolicyRecord{}, nil
}
func (s *radarGovernanceHandlerRepoStub) CreateReleaseSubject(_ context.Context, input service.ReleaseSubjectInput) (*service.ReleaseSubjectRecord, error) {
	s.releaseSubject = &input
	return &service.ReleaseSubjectRecord{ID: uuid.New(), RunID: input.RunID}, nil
}
func (s *radarGovernanceHandlerRepoStub) ActivateReleaseSubject(_ context.Context, input service.ReleaseSubjectActivationInput) (*service.ReleaseSubjectEvent, error) {
	return &service.ReleaseSubjectEvent{ReleaseSubjectID: input.ReleaseSubjectID, EventType: "activated"}, nil
}
func (s *radarGovernanceHandlerRepoStub) RevokeReleaseSubject(_ context.Context, id uuid.UUID, actorID int64) (*service.ReleaseSubjectEvent, error) {
	return &service.ReleaseSubjectEvent{ReleaseSubjectID: id, EventType: "revoked", ActorID: actorID}, nil
}
func (s *radarGovernanceHandlerRepoStub) ActivateGatePolicy(_ context.Context, input service.RadarGatePolicyActivationInput) (*service.RadarGatePolicyHead, error) {
	s.policyActivation = &input
	return &service.RadarGatePolicyHead{PolicyID: input.PolicyID, Scope: input.Scope}, nil
}
func (s *radarGovernanceHandlerRepoStub) ActivateBaselineHead(_ context.Context, input service.RadarBaselineActivationInput) (*service.RadarBaselineHead, error) {
	s.baselineActivation = &input
	return &service.RadarBaselineHead{BaselineID: input.BaselineID, Scope: input.Scope}, nil
}
func (s *radarGovernanceHandlerRepoStub) RecordGateDecision(_ context.Context, input service.RadarGateDecisionInput) (*service.RadarGateDecisionRecord, error) {
	s.gateDecision = &input
	return &service.RadarGateDecisionRecord{Status: input.Status, RuleIDs: input.RuleIDs}, nil
}
func (s *radarGovernanceHandlerRepoStub) WaiveGateDecision(context.Context, service.RadarGateWaiverInput) (*service.RadarGateWaiverRecord, error) {
	return &service.RadarGateWaiverRecord{}, nil
}
func (s *radarGovernanceHandlerRepoStub) RotateEvidenceSigningKey(_ context.Context, input service.RotateEvidenceSigningKeyInput) (*service.EvidenceSigningKeyRecord, error) {
	return &service.EvidenceSigningKeyRecord{ID: input.ID, KeyReference: input.KeyReference, Status: service.EvidenceSigningKeyActive, StateEpoch: 1}, nil
}
func (s *radarGovernanceHandlerRepoStub) TransitionEvidenceSigningKey(_ context.Context, input service.TransitionEvidenceSigningKeyInput) (*service.EvidenceSigningKeyRecord, error) {
	return &service.EvidenceSigningKeyRecord{ID: input.ID, Status: input.Status, StateEpoch: input.ExpectedStateEpoch + 1}, nil
}
func (s *radarGovernanceHandlerRepoStub) ObserveAlert(context.Context, service.RadarAlertObservationInput) (*service.RadarAlertRecord, error) {
	return &service.RadarAlertRecord{}, nil
}
func (s *radarGovernanceHandlerRepoStub) AcknowledgeAlert(context.Context, uuid.UUID, int64) error {
	return nil
}
func (s *radarGovernanceHandlerRepoStub) RecordAlertRecovery(context.Context, uuid.UUID, uuid.UUID, bool, int64, json.RawMessage) error {
	return nil
}
func (s *radarGovernanceHandlerRepoStub) ResolveAlert(context.Context, uuid.UUID, int64) error {
	return nil
}
func (s *radarGovernanceHandlerRepoStub) RecordAttribution(context.Context, service.RadarAttributionInput) (*service.RadarAttributionRecord, error) {
	return &service.RadarAttributionRecord{}, nil
}
func (s *radarGovernanceHandlerRepoStub) GetAlert(context.Context, uuid.UUID) (*service.RadarAlertRecord, error) {
	return &service.RadarAlertRecord{}, nil
}

func radarGovernanceTestContext(userID int64) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
	return c
}

func TestRadarGovernanceHandlerUsesAuthenticatedActorForBaselineProposal(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"model_route":"deepseek","run_id":"`+uuid.NewString()+`","dataset_manifest_sha256":"`+string(bytes.Repeat([]byte("a"), 64))+`","evidence_hash":"`+string(bytes.Repeat([]byte("b"), 64))+`","route_profile_version":"v1","policy_version":1}`))
	h.ProposeBaseline(c)
	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.NotNil(t, repo.proposed)
	require.Equal(t, int64(77), repo.proposed.ProposedBy)
}

func TestRadarGovernanceHandlerRejectsMissingRadarPermission(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleViewer}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`))
	h.CreateRoleBinding(c)
	require.Equal(t, http.StatusForbidden, c.Writer.Status())
}

func TestRadarGovernanceHandlerDerivesGateStatusServerSide(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleReleaseManager}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"run_id":"`+uuid.NewString()+`",
		"policy_id":"`+uuid.NewString()+`",
		"policy":{"version":1,"observation_days":14,"enforcement_starts_at":"2026-01-01T00:00:00Z","critical_domain_delta_pp":-3,"aggregate_delta_pp":-2,"confidence_level":0.95,"require_ci_exclude_zero":true},
		"input":{"evidence_sufficient":false,"route_evidence_present":true,"route_match":true,"observed_at":"2026-07-27T00:00:00Z","observation_days":14},
		"evidence_hash":"`+string(bytes.Repeat([]byte("c"), 64))+`"
	}`))
	h.EvaluateGate(c)
	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.NotNil(t, repo.gateDecision)
	require.Equal(t, service.RadarGateInsufficientEvidence, repo.gateDecision.Status)
	require.Equal(t, []string{"evidence.sufficient"}, repo.gateDecision.RuleIDs)
}

func TestRadarGovernanceHandlerCreatesDatasetWithAuthenticatedActor(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"dataset_key":"synthetic-smoke","version":"v1","source_type":"synthetic",
		"cases":[{"case_key":"echo","capability_domain":"instruction","priority":"P0","weight":"1","sample_count":1,"prompt_spec":{"input":"ping"},"expected_spec":{"output":"pong"},"execution_spec":{"url":"/v1/responses"},"grader_id":"exact","grader_version":"v1","confidentiality":"synthetic","estimated_cost":"0.01"}]
	}`))

	h.CreateDataset(c)

	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.NotNil(t, repo.dataset)
	require.Equal(t, int64(77), repo.dataset.CreatedBy)
	require.Equal(t, "synthetic-smoke", repo.dataset.DatasetKey)
	require.Len(t, repo.dataset.Cases, 1)
}

func TestRadarGovernanceHandlerStartsRunWithAuthenticatedActor(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleTestOperator}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	planID := uuid.New()
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"plan_id":"`+planID.String()+`","trigger_source":"manual","baseline_ref":{"revision":"base"},"candidate_ref":{"revision":"candidate"}}`))

	h.StartRun(c)

	require.Equal(t, http.StatusAccepted, c.Writer.Status())
	require.NotNil(t, repo.run)
	require.Equal(t, int64(77), repo.run.CreatedBy)
	require.Equal(t, planID, repo.run.PlanID)
}

func TestRadarGovernanceHandlerEnablesEvaluationKeyWithAuthenticatedActor(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RolePlatformAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: "42"}}

	h.EnableEvaluationKey(c)

	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.Equal(t, int64(42), repo.evaluationKeyID)
	require.Equal(t, int64(77), repo.evaluationKeyActorID)
}

func TestRadarGovernanceHandlerControlsRunWithPermissionAndIdempotencyKey(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleTestOperator}})
	repo := &radarRunControlHandlerRepoStub{radarGovernanceHandlerRepoStub: radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}}
	h := NewRadarGovernanceHandler(repo)
	runID := uuid.New()
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: runID.String()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"reason":"operator"}`))
	c.Request.Header.Set("Idempotency-Key", strings.Repeat("a", 64))
	h.PauseRun(c)
	require.Equal(t, http.StatusOK, c.Writer.Status())
}

func TestRadarGovernanceHandlerCreatesRevisionBatchWithAuthenticatedActor(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleTestOperator}})
	repo := &radarRevisionBatchHandlerRepoStub{radarGovernanceHandlerRepoStub: radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}}
	h := NewRadarGovernanceHandler(repo)
	runID := uuid.New()
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"run_id":"`+runID.String()+`","reason":"model regression"}`))
	c.Request.Header.Set("Idempotency-Key", strings.Repeat("a", 64))

	h.CreateRevisionBatch(c)

	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.NotNil(t, repo.created)
	require.Equal(t, runID, repo.created.RunID)
	require.Equal(t, int64(77), repo.created.RequestedBy)
	require.Equal(t, strings.Repeat("a", 64), repo.created.IdempotencyKey)
}

func TestRadarGovernanceHandlerRevisionBatchControlsUseExpectedAction(t *testing.T) {
	for _, action := range []string{"fence", "resume", "cancel", "repair"} {
		t.Run(action, func(t *testing.T) {
			auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleTestOperator}})
			repo := &radarRevisionBatchHandlerRepoStub{radarGovernanceHandlerRepoStub: radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}}
			h := NewRadarGovernanceHandler(repo)
			batchID := uuid.New()
			c := radarGovernanceTestContext(77)
			c.Params = gin.Params{{Key: "id", Value: batchID.String()}}
			c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"reason":"operator action"}`))
			c.Request.Header.Set("Idempotency-Key", strings.Repeat("b", 64))
			switch action {
			case "fence":
				h.FenceRevisionBatch(c)
			case "resume":
				h.ResumeRevisionBatch(c)
			case "cancel":
				h.CancelRevisionBatch(c)
			case "repair":
				h.RepairRevisionBatch(c)
			}
			require.Equal(t, http.StatusOK, c.Writer.Status())
			require.Equal(t, action, repo.controlAction)
			require.NotNil(t, repo.control)
			require.Equal(t, batchID, repo.control.BatchID)
			require.Equal(t, int64(77), repo.control.ActorID)
		})
	}
}

func TestRadarGovernanceHandlerApprovesCompensatingHeadWithQualityPermission(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarRevisionBatchHandlerRepoStub{
		radarGovernanceHandlerRepoStub: radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth},
		compensatingResult:             service.CompensatingScoreHeadResult{ApprovalCount: 1},
	}
	h := NewRadarGovernanceHandler(repo)
	batchID, sampleID, scoreID := uuid.New(), uuid.New(), uuid.New()
	createdAt := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: batchID.String()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"sample_id":"`+sampleID.String()+`","grader_id":"grader",
		"score_ref":{"score_id":"`+scoreID.String()+`","score_created_at":"`+createdAt.Format(time.RFC3339)+`"}
	}`))
	c.Request.Header.Set("Idempotency-Key", strings.Repeat("c", 64))

	h.ApproveCompensatingScoreHead(c)

	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.NotNil(t, repo.compensating)
	require.Equal(t, batchID, repo.compensating.BatchID)
	require.Equal(t, sampleID, repo.compensating.SampleID)
	require.Equal(t, service.ScoreRef{ID: scoreID, CreatedAt: createdAt}, repo.compensating.ScoreRef)
	require.Equal(t, int64(77), repo.compensating.ActorID)
}

func TestRadarGovernanceHandlerRejectsMalformedRevisionIdempotencyKey(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleTestOperator}})
	repo := &radarRevisionBatchHandlerRepoStub{radarGovernanceHandlerRepoStub: radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"run_id":"`+uuid.NewString()+`","reason":"model regression"}`))
	c.Request.Header.Set("Idempotency-Key", strings.Repeat("g", 64))

	h.CreateRevisionBatch(c)

	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
	require.Nil(t, repo.created)
}

func TestRadarGovernanceHandlerWorkerRegistrationReturnsFingerprintOnly(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleTestOperator}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 77})
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"name":"runner-a","worker_kind":"runner","token":"worker-secret","capabilities":["chat"]}`))
	c.Request.Header.Set("Idempotency-Key", strings.Repeat("a", 64))
	h.RegisterWorker(c)
	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.NotContains(t, recorder.Body.String(), "worker-secret")
	require.Contains(t, recorder.Body.String(), "abcd1234efgh")
}

func TestRadarGovernanceHandlerActivatesPolicyWithAuthenticatedActorAndScope(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	policyID := uuid.New()
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: policyID.String()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"environment":"production","scope_type":"global","scope_id":"global"}`))

	h.ActivateGatePolicy(c)

	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.NotNil(t, repo.policyActivation)
	require.Equal(t, policyID, repo.policyActivation.PolicyID)
	require.Equal(t, int64(77), repo.policyActivation.ActorID)
}

func TestRadarGovernanceHandlerActivatesBaselineHeadWithScope(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleReleaseManager}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	baselineID := uuid.New()
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: baselineID.String()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"environment":"staging","scope_type":"route","scope_id":"route-a"}`))

	h.ActivateBaseline(c)

	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.NotNil(t, repo.baselineActivation)
	require.Equal(t, baselineID, repo.baselineActivation.BaselineID)
	require.Equal(t, int64(77), repo.baselineActivation.ActorID)
}
