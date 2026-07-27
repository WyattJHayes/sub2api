package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type radarGovernanceHandlerRepoStub struct {
	service.StaticRadarAuthorizer
	proposed             *service.RadarBaselineInput
	gateDecision         *service.RadarGateDecisionInput
	dataset              *service.CreateRadarDatasetInput
	plan                 *service.CreateRadarPlanInput
	run                  *service.CreateRunInput
	evaluationKeyID      int64
	evaluationKeyActorID int64
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
func (s *radarGovernanceHandlerRepoStub) RecordGateDecision(_ context.Context, input service.RadarGateDecisionInput) (*service.RadarGateDecisionRecord, error) {
	s.gateDecision = &input
	return &service.RadarGateDecisionRecord{Status: input.Status, RuleIDs: input.RuleIDs}, nil
}
func (s *radarGovernanceHandlerRepoStub) WaiveGateDecision(context.Context, service.RadarGateWaiverInput) (*service.RadarGateWaiverRecord, error) {
	return &service.RadarGateWaiverRecord{}, nil
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
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: *auth}
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
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: *auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`))
	h.CreateRoleBinding(c)
	require.Equal(t, http.StatusForbidden, c.Writer.Status())
}

func TestRadarGovernanceHandlerDerivesGateStatusServerSide(t *testing.T) {
	auth := service.NewStaticRadarAuthorizer(map[int64][]service.RadarRole{77: {service.RoleReleaseManager}})
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: *auth}
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
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: *auth}
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
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: *auth}
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
	repo := &radarGovernanceHandlerRepoStub{StaticRadarAuthorizer: *auth}
	h := NewRadarGovernanceHandler(repo)
	c := radarGovernanceTestContext(77)
	c.Params = gin.Params{{Key: "id", Value: "42"}}

	h.EnableEvaluationKey(c)

	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.Equal(t, int64(42), repo.evaluationKeyID)
	require.Equal(t, int64(77), repo.evaluationKeyActorID)
}
