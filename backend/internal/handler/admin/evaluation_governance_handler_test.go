package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	proposed                  *service.RadarBaselineInput
	gateDecision              *service.RadarGateDecisionInput
	gatePolicy                *service.RadarGatePolicyInput
	gatePolicyApproval        *service.RadarGatePolicyApprovalInput
	gatePolicyApprovalErr     error
	releaseSubject            *service.ReleaseSubjectInput
	policyActivation          *service.RadarGatePolicyActivationInput
	baselineActivation        *service.RadarBaselineActivationInput
	dataset                   *service.CreateRadarDatasetInput
	plan                      *service.CreateRadarPlanInput
	run                       *service.CreateRunInput
	evaluationKeyID           int64
	evaluationKeyActorID      int64
	gateReliability           *service.RadarGateReliabilityContext
	gateReliabilityRunID      uuid.UUID
	gateReliabilityPolicyID   uuid.UUID
	gateReliabilityObservedAt time.Time
	reliabilityFacts          *service.RadarReliabilityFacts
	createdRoleBindingTenant  int64
	createdRoleBindingActor   int64
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

func (s *radarGovernanceHandlerRepoStub) CreateRoleBinding(ctx context.Context, input service.RadarRoleBindingInput) (*service.RadarRoleBinding, error) {
	s.createdRoleBindingTenant, _ = middleware.RadarTenantID(ctx)
	s.createdRoleBindingActor = input.ActorID
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
func (s *radarGovernanceHandlerRepoStub) CreateGatePolicy(_ context.Context, input service.RadarGatePolicyInput) (*service.RadarGatePolicyRecord, error) {
	s.gatePolicy = &input
	return &service.RadarGatePolicyRecord{PolicyHash: input.PolicyHash}, nil
}
func (s *radarGovernanceHandlerRepoStub) ApproveGatePolicy(_ context.Context, input service.RadarGatePolicyApprovalInput) (*service.RadarGatePolicyApprovalRecord, error) {
	s.gatePolicyApproval = &input
	if s.gatePolicyApprovalErr != nil {
		return nil, s.gatePolicyApprovalErr
	}
	return &service.RadarGatePolicyApprovalRecord{
		PolicyID: input.PolicyID, ApproverID: input.ApproverID, Role: input.Role,
		PolicyHash: input.PolicyHash, EvidenceHash: input.EvidenceHash,
		EffectiveAt: input.EffectiveAt, ExpiresAt: input.ExpiresAt,
	}, nil
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
func (s *radarGovernanceHandlerRepoStub) LoadRadarGateReliability(_ context.Context, runID, policyID uuid.UUID) (*service.RadarGateReliabilityContext, error) {
	s.gateReliabilityRunID = runID
	s.gateReliabilityPolicyID = policyID
	if s.gateReliability != nil {
		s.gateReliabilityObservedAt = s.gateReliability.ObservedAt
	}
	return s.gateReliability, nil
}
func (s *radarGovernanceHandlerRepoStub) GetReliabilityFacts(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ string) (*service.RadarReliabilityFacts, error) {
	return s.reliabilityFacts, nil
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

func TestRadarGovernanceHandlerPropagatesAuthenticatedTenant(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RolePlatformAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"actor_id":999,"role":"viewer"}`))
	h.CreateRoleBinding(c)
	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.Equal(t, int64(77), repo.createdRoleBindingTenant)
	require.Equal(t, int64(999), repo.createdRoleBindingActor)
}

func TestRadarGovernanceHandlerDefaultsRoleBindingActorToAuthenticatedUser(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RolePlatformAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"role":"viewer"}`))
	h.CreateRoleBinding(c)
	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.Equal(t, int64(77), repo.createdRoleBindingActor)
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

func TestRadarGovernanceHandlerComputesGatePolicyHashServerSide(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	policy := json.RawMessage(`{"aggregate_delta_pp":-2,"observation_days":14}`)
	wantHash, err := service.DigestCanonicalJSON(policy)
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"version":9,
		"policy":`+string(policy)+`,
		"policy_hash":"`+strings.Repeat("f", 64)+`",
		"enforcement_starts_at":"2026-08-01T00:00:00Z",
		"approval_expires_at":"2026-08-02T00:00:00Z"
	}`))

	h.CreateGatePolicy(c)

	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.NotNil(t, repo.gatePolicy)
	require.Equal(t, wantHash, repo.gatePolicy.PolicyHash)
	require.NotEqual(t, strings.Repeat("f", 64), repo.gatePolicy.PolicyHash)
}

func TestRadarGovernanceHandlerAcceptsGatePolicyWithoutClientHash(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	policy := json.RawMessage(`{"observation_days":14}`)
	wantHash, err := service.DigestCanonicalJSON(policy)
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"version":10,
		"policy":`+string(policy)+`,
		"enforcement_starts_at":"2026-08-01T00:00:00Z",
		"approval_expires_at":"2026-08-02T00:00:00Z"
	}`))

	h.CreateGatePolicy(c)

	require.Equal(t, http.StatusCreated, c.Writer.Status())
	require.NotNil(t, repo.gatePolicy)
	require.Equal(t, wantHash, repo.gatePolicy.PolicyHash)
}

func TestRadarGovernanceHandlerReadsTenantScopedReliabilityFacts(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleViewer}})
	runID, policyID := uuid.New(), uuid.New()
	repo := &radarGovernanceHandlerRepoStub{
		StaticRadarAuthorizer: auth,
		reliabilityFacts:      &service.RadarReliabilityFacts{RunID: runID, PolicyID: policyID, ProfileID: "staging-v1"},
	}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: runID.String()}}
	c.Request = httptest.NewRequest(http.MethodGet, "/?policy_id="+policyID.String()+"&profile_id=staging-v1", nil)
	h.GetReliabilityFacts(c)
	require.Equal(t, http.StatusOK, c.Writer.Status())
}

func TestRadarGovernanceHandlerApprovesGatePolicyWithAuthenticatedActor(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	policyID := uuid.New()
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: policyID.String()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"role":"quality_admin",
		"policy_hash":"`+strings.Repeat("a", 64)+`",
		"evidence_hash":"`+strings.Repeat("b", 64)+`",
		"effective_at":"2026-07-30T00:00:00Z",
		"expires_at":"2026-08-02T00:00:00Z"
	}`))

	h.ApproveGatePolicy(c)

	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.NotNil(t, repo.gatePolicyApproval)
	require.Equal(t, policyID, repo.gatePolicyApproval.PolicyID)
	require.Equal(t, int64(77), repo.gatePolicyApproval.ApproverID)
	require.Equal(t, service.RoleQualityAdmin, repo.gatePolicyApproval.Role)
}

func TestRadarGovernanceHandlerRejectsGatePolicyApprovalRole(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: uuid.NewString()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"role":"viewer",
		"policy_hash":"`+strings.Repeat("a", 64)+`",
		"evidence_hash":"`+strings.Repeat("b", 64)+`",
		"effective_at":"2026-07-30T00:00:00Z",
		"expires_at":"2026-08-02T00:00:00Z"
	}`))

	h.ApproveGatePolicy(c)

	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
	require.Nil(t, repo.gatePolicyApproval)
}

func TestRadarGovernanceHandlerRejectsGatePolicyApprovalWithoutPermission(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleViewer}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: uuid.NewString()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"role":"quality_admin",
		"policy_hash":"`+strings.Repeat("a", 64)+`",
		"evidence_hash":"`+strings.Repeat("b", 64)+`",
		"effective_at":"2026-07-30T00:00:00Z",
		"expires_at":"2026-08-02T00:00:00Z"
	}`))

	h.ApproveGatePolicy(c)

	require.Equal(t, http.StatusForbidden, c.Writer.Status())
	require.Nil(t, repo.gatePolicyApproval)
}

func TestRadarGovernanceHandlerPropagatesCreatorSelfApprovalRejection(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleQualityAdmin}})
	repo := &radarGovernanceHandlerRepoStub{
		StaticRadarAuthorizer: auth,
		gatePolicyApprovalErr: errors.New("gate policy creator cannot approve the same policy"),
	}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: uuid.NewString()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"role":"quality_admin",
		"policy_hash":"`+strings.Repeat("a", 64)+`",
		"evidence_hash":"`+strings.Repeat("b", 64)+`",
		"effective_at":"2026-07-30T00:00:00Z",
		"expires_at":"2026-08-02T00:00:00Z"
	}`))

	h.ApproveGatePolicy(c)

	require.Equal(t, http.StatusInternalServerError, c.Writer.Status())
	require.NotNil(t, repo.gatePolicyApproval)
}

func TestRadarGovernanceHandlerDerivesGateStatusServerSide(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleReleaseManager}})
	repo := &radarGovernanceHandlerRepoStub{
		StaticRadarAuthorizer: auth,
		gateReliability: &service.RadarGateReliabilityContext{
			Policy: service.RadarGatePolicy{
				Version: 1, ObservationDays: 14, EnforcementStartsAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			InputLoaded: true,
			Input: service.RadarGateInput{
				EvidenceSufficient: false, RouteEvidencePresent: true, RouteMatch: true,
				ObservationDays: 14,
			},
		},
	}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"run_id":"`+uuid.NewString()+`",
		"policy_id":"`+uuid.NewString()+`",
		"policy":{"version":1,"observation_days":14,"enforcement_starts_at":"2026-01-01T00:00:00Z","critical_domain_delta_pp":-3,"aggregate_delta_pp":-2,"confidence_level":0.95,"require_ci_exclude_zero":true},
		"input":{"evidence_sufficient":true,"route_evidence_present":false,"route_match":false,"observed_at":"2026-07-27T00:00:00Z","observation_days":0,"new_p0_failure":true,"critical_delta_pp":-100,"aggregate_delta_pp":-100,"judge_disagreement":true},
		"evidence_hash":"`+string(bytes.Repeat([]byte("c"), 64))+`"
	}`))
	h.EvaluateGate(c)
	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.NotNil(t, repo.gateDecision)
	require.Equal(t, service.RadarGateInsufficientEvidence, repo.gateDecision.Status)
	require.Equal(t, []string{"evidence.sufficient"}, repo.gateDecision.RuleIDs)
}

func TestRadarGovernanceHandlerLoadsAuthoritativeReliabilityForGate(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleReleaseManager}})
	runID := uuid.New()
	policyID := uuid.New()
	supersedes := uuid.New()
	observedAt := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	sourceWatermark := json.RawMessage(`{"version":"radar-gate-reliability-watermark-v1","snapshot_refs":[{"snapshot_id":"` + uuid.NewString() + `"}]}`)
	repo := &radarGovernanceHandlerRepoStub{
		StaticRadarAuthorizer: auth,
		gateReliability: &service.RadarGateReliabilityContext{
			Policy: service.RadarGatePolicy{
				Version: 7, ObservationDays: 14, EnforcementStartsAt: observedAt.Add(-time.Hour),
				RequireReliability: true, MaxP99LatencyMS: 500,
			},
			ObservedAt:  observedAt,
			InputLoaded: true,
			Input: service.RadarGateInput{
				EvidenceSufficient: true, RouteEvidencePresent: true, RouteMatch: true,
				ObservationDays: 14,
			},
			Evidence: service.RadarGateReliabilityEvidence{
				HeadPresent: true, Current: true, Fresh: true, DenominatorComplete: true,
				HistogramIntegrityValid: true, SourceWatermarkValid: true, QueryVersionAllowed: true,
				BillingReconciled: true, MaxP99LatencyMS: 501,
			},
			PolicyHash: strings.Repeat("d", 64), ReleaseSubjectHash: strings.Repeat("e", 64),
			SourceWatermark: sourceWatermark, SupersedesDecisionID: &supersedes,
		},
	}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"run_id":"`+runID.String()+`",
		"policy_id":"`+policyID.String()+`",
		"policy":{"version":1,"observation_days":1,"enforcement_starts_at":"2027-01-01T00:00:00Z"},
		"input":{"evidence_sufficient":true,"route_evidence_present":true,"route_match":true,"observed_at":"2020-01-01T00:00:00Z","observation_days":14,"reliability_slo_breached":false},
		"evidence":{"caller":"untrusted"},
		"evidence_hash":"`+strings.Repeat("c", 64)+`"
	}`))

	h.EvaluateGate(c)

	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.Equal(t, runID, repo.gateReliabilityRunID)
	require.Equal(t, policyID, repo.gateReliabilityPolicyID)
	require.Equal(t, observedAt, repo.gateReliabilityObservedAt)
	require.NotNil(t, repo.gateDecision)
	require.Equal(t, service.RadarGateBlocked, repo.gateDecision.Status)
	require.Equal(t, []string{"slo.reliability.p99"}, repo.gateDecision.RuleIDs)
	require.Equal(t, strings.Repeat("e", 64), repo.gateDecision.ReleaseSubjectHash)
	require.JSONEq(t, string(sourceWatermark), string(repo.gateDecision.SourceWatermark))
	require.Equal(t, &supersedes, repo.gateDecision.SupersedesDecisionID)
	require.Len(t, repo.gateDecision.EvidenceHash, 64)
	require.NotEqual(t, strings.Repeat("c", 64), repo.gateDecision.EvidenceHash)
	var evidence map[string]any
	require.NoError(t, json.Unmarshal(repo.gateDecision.Evidence, &evidence))
	require.Equal(t, "radar-gate-evidence-v1", evidence["version"])
	require.Equal(t, strings.Repeat("d", 64), evidence["policy_hash"])
	require.Equal(t, observedAt.Format(time.RFC3339Nano), evidence["observed_at"])
}

func TestRadarGovernanceHandlerRejectsDirectGateDecision(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleReleaseManager}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: auth}
	h := NewRadarGovernanceHandler(repo)
	runID := uuid.New()
	policyID := uuid.New()
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{
		"run_id":"`+runID.String()+`",
		"policy_id":"`+policyID.String()+`",
		"status":"passed",
		"rule_ids":["pass"],
		"evidence":{},
		"evidence_hash":"`+strings.Repeat("a", 64)+`"
	}`))

	h.RecordGateDecision(c)

	require.Equal(t, http.StatusConflict, c.Writer.Status())
	require.Nil(t, repo.gateDecision)
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
