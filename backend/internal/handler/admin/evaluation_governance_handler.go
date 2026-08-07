package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RadarGovernanceHandler exposes the auditable Radar control-plane actions.
// The authenticated subject supplies the audit actor. Role bindings may target
// another subject after the repository verifies the tenant boundary.
type RadarGovernanceHandler struct {
	repo service.RadarGovernanceRepository
}

func NewRadarGovernanceHandler(repo service.RadarGovernanceRepository) *RadarGovernanceHandler {
	return &RadarGovernanceHandler{repo: repo}
}

func (h *RadarGovernanceHandler) actor(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not authenticated")
		return 0, false
	}
	tenantID := subject.TenantID
	if tenantID <= 0 {
		tenantID = subject.UserID
	}
	if c.Request != nil {
		ctx := middleware.WithRadarTenant(c.Request.Context(), tenantID)
		ctx = middleware.WithRadarActor(ctx, subject.UserID)
		c.Request = c.Request.WithContext(ctx)
	}
	return subject.UserID, true
}

func (h *RadarGovernanceHandler) require(c *gin.Context, permission service.RadarPermission) (int64, bool) {
	if h == nil || h.repo == nil {
		response.Error(c, http.StatusServiceUnavailable, "Radar governance is not available")
		return 0, false
	}
	actorID, ok := h.actor(c)
	if !ok {
		return 0, false
	}
	if err := h.repo.Require(c.Request.Context(), actorID, permission); err != nil {
		if errors.Is(err, service.ErrRadarForbidden) {
			response.Forbidden(c, "Radar permission denied")
		} else {
			response.ErrorFrom(c, err)
		}
		return 0, false
	}
	return actorID, true
}

func decodeJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return false
	}
	return true
}

func rawOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(c.Param(name)))
	if err != nil {
		response.BadRequest(c, "Invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

func validateRadarRole(role service.RadarRole) bool {
	switch role {
	case service.RoleViewer, service.RoleTestOperator, service.RoleQualityAdmin,
		service.RoleReleaseManager, service.RolePlatformAdmin:
		return true
	default:
		return false
	}
}

func parseOptionalInt64(c *gin.Context, name string) (*int64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid "+name)
		return nil, false
	}
	return &id, true
}

func parseInt64Param(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param(name)), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid "+name)
		return 0, false
	}
	return id, true
}

func (h *RadarGovernanceHandler) EnableEvaluationKey(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionEvaluationKeyManage)
	if !ok {
		return
	}
	keyID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	key, err := h.repo.EnableEvaluationKey(c.Request.Context(), keyID, actorID)
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "Active API key not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, key)
}

func (h *RadarGovernanceHandler) CreateDataset(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionDatasetManage)
	if !ok {
		return
	}
	var req service.CreateRadarDatasetInput
	if !decodeJSON(c, &req) {
		return
	}
	req.CreatedBy = actorID
	dataset, err := h.repo.CreateDataset(c.Request.Context(), req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, dataset)
}

func (h *RadarGovernanceHandler) PublishDataset(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionDatasetPublish)
	if !ok {
		return
	}
	datasetID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	dataset, err := h.repo.PublishDataset(c.Request.Context(), datasetID, actorID)
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "Draft dataset not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dataset)
}

func (h *RadarGovernanceHandler) CreatePlan(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionDatasetManage)
	if !ok {
		return
	}
	var req service.CreateRadarPlanInput
	if !decodeJSON(c, &req) {
		return
	}
	req.CreatedBy = actorID
	plan, err := h.repo.CreatePlan(c.Request.Context(), req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, plan)
}

func (h *RadarGovernanceHandler) StartRun(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionRunStart)
	if !ok {
		return
	}
	var req struct {
		PlanID        uuid.UUID      `json:"plan_id" binding:"required"`
		TriggerSource string         `json:"trigger_source" binding:"required"`
		BaselineRef   map[string]any `json:"baseline_ref"`
		CandidateRef  map[string]any `json:"candidate_ref"`
	}
	if !decodeJSON(c, &req) {
		return
	}
	run, err := h.repo.CreateRunWithMatrix(c.Request.Context(), service.CreateRunInput{
		PlanID: req.PlanID, TriggerSource: req.TriggerSource, BaselineRef: req.BaselineRef,
		CandidateRef: req.CandidateRef, CreatedBy: actorID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Accepted(c, run)
}

type radarRoleBindingRequest struct {
	ActorID int64             `json:"actor_id" binding:"omitempty,gt=0"`
	Role    service.RadarRole `json:"role" binding:"required"`
	Scope   json.RawMessage   `json:"scope"`
}

func (h *RadarGovernanceHandler) ListRoleBindings(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionRoleManage); !ok {
		return
	}
	actorID, ok := parseOptionalInt64(c, "actor_id")
	if !ok {
		return
	}
	bindings, err := h.repo.ListRoleBindings(c.Request.Context(), actorID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, bindings)
}

func (h *RadarGovernanceHandler) CreateRoleBinding(c *gin.Context) {
	createdBy, ok := h.require(c, service.PermissionRoleManage)
	if !ok {
		return
	}
	var req radarRoleBindingRequest
	if !decodeJSON(c, &req) {
		return
	}
	if !validateRadarRole(req.Role) {
		response.BadRequest(c, "Invalid radar role")
		return
	}
	targetActorID := req.ActorID
	if targetActorID <= 0 {
		targetActorID = createdBy
	}
	binding, err := h.repo.CreateRoleBinding(c.Request.Context(), service.RadarRoleBindingInput{
		ActorID: targetActorID, Role: req.Role, Scope: rawOrEmpty(req.Scope), CreatedBy: createdBy,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, binding)
}

func (h *RadarGovernanceHandler) DisableRoleBinding(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionRoleManage)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	if err := h.repo.DisableRoleBinding(c.Request.Context(), id, actorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "Role binding not found")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"id": id, "enabled": false})
}

type radarBaselineProposalRequest struct {
	ModelRoute            string    `json:"model_route" binding:"required"`
	RunID                 uuid.UUID `json:"run_id" binding:"required"`
	DatasetManifestSHA256 string    `json:"dataset_manifest_sha256" binding:"required,len=64"`
	EvidenceHash          string    `json:"evidence_hash" binding:"required,len=64"`
	RouteProfileVersion   string    `json:"route_profile_version" binding:"required"`
	PolicyVersion         int       `json:"policy_version" binding:"required,gt=0"`
}

func (h *RadarGovernanceHandler) ProposeBaseline(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionBaselineQualityApprove)
	if !ok {
		return
	}
	var req radarBaselineProposalRequest
	if !decodeJSON(c, &req) {
		return
	}
	baseline, err := h.repo.ProposeBaseline(c.Request.Context(), service.RadarBaselineInput{
		ModelRoute: req.ModelRoute, RunID: req.RunID, DatasetManifestSHA256: req.DatasetManifestSHA256,
		EvidenceHash: req.EvidenceHash, RouteProfileVersion: req.RouteProfileVersion,
		PolicyVersion: req.PolicyVersion, ProposedBy: actorID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, baseline)
}

func (h *RadarGovernanceHandler) GetBaseline(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionView); !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	baseline, err := h.repo.GetBaseline(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "Baseline not found")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, baseline)
}

type radarBaselineApprovalRequest struct {
	Role         service.RadarRole `json:"role" binding:"required"`
	EvidenceHash string            `json:"evidence_hash" binding:"required,len=64"`
	ExpiresAt    time.Time         `json:"expires_at" binding:"required"`
}

func (h *RadarGovernanceHandler) ApproveBaseline(c *gin.Context) {
	actorID, ok := h.actor(c)
	if !ok || h == nil || h.repo == nil {
		return
	}
	var req radarBaselineApprovalRequest
	if !decodeJSON(c, &req) {
		return
	}
	var permission service.RadarPermission
	switch req.Role {
	case service.RoleQualityAdmin:
		permission = service.PermissionBaselineQualityApprove
	case service.RoleReleaseManager:
		permission = service.PermissionBaselineReleaseApprove
	default:
		response.BadRequest(c, "Approval role must be quality_admin or release_manager")
		return
	}
	if err := h.repo.Require(c.Request.Context(), actorID, permission); err != nil {
		if errors.Is(err, service.ErrRadarForbidden) {
			response.Forbidden(c, "Radar permission denied")
		} else {
			response.ErrorFrom(c, err)
		}
		return
	}
	baselineID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	baseline, err := h.repo.GetBaseline(c.Request.Context(), baselineID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "Baseline not found")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	if baseline.ProposedBy == actorID {
		response.Forbidden(c, "Baseline proposer cannot approve the same baseline")
		return
	}
	approval, err := h.repo.ApproveBaseline(c.Request.Context(), service.RadarBaselineApprovalInput{
		BaselineID: baselineID, ApproverID: actorID, Role: req.Role, EvidenceHash: req.EvidenceHash,
		EffectiveAt: time.Now().UTC(), ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, approval)
}

func (h *RadarGovernanceHandler) ActivateBaseline(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionBaselineReleaseApprove)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Environment        string     `json:"environment" binding:"required"`
		ScopeType          string     `json:"scope_type" binding:"required"`
		ScopeID            string     `json:"scope_id" binding:"required"`
		ExpectedBaselineID *uuid.UUID `json:"expected_baseline_id"`
	}
	if !decodeJSON(c, &req) {
		return
	}
	baseline, err := h.repo.ActivateBaselineHead(c.Request.Context(), service.RadarBaselineActivationInput{
		BaselineID: id,
		Scope: service.RadarGovernanceScope{
			Environment: req.Environment,
			ScopeType:   req.ScopeType,
			ScopeID:     req.ScopeID,
		},
		ActorID: actorID, ExpectedBaselineID: req.ExpectedBaselineID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, baseline)
}

type radarReleaseSubjectRequest struct {
	RunID        uuid.UUID              `json:"run_id" binding:"required"`
	Subject      service.ReleaseSubject `json:"subject" binding:"required"`
	ExpectedHash string                 `json:"expected_hash"`
}

func (h *RadarGovernanceHandler) CreateReleaseSubject(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionGateDecide); !ok {
		return
	}
	var req radarReleaseSubjectRequest
	if !decodeJSON(c, &req) {
		return
	}
	record, err := h.repo.CreateReleaseSubject(c.Request.Context(), service.ReleaseSubjectInput{
		RunID: req.RunID, Subject: req.Subject, ExpectedHash: req.ExpectedHash,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, record)
}

func (h *RadarGovernanceHandler) GetReleaseSubject(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionView); !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	reader, ok := h.repo.(service.RadarReleaseSubjectRepository)
	if !ok {
		response.Error(c, http.StatusServiceUnavailable, "Radar release subject reader is not available")
		return
	}
	record, err := reader.GetReleaseSubject(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "Release subject not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, record)
}

func (h *RadarGovernanceHandler) GetReliabilityFacts(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionView); !ok {
		return
	}
	runID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	policyID, err := uuid.Parse(strings.TrimSpace(c.Query("policy_id")))
	if err != nil || policyID == uuid.Nil {
		response.BadRequest(c, "Invalid policy_id")
		return
	}
	profileID := strings.TrimSpace(c.Query("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "profile_id is required")
		return
	}
	reader, ok := h.repo.(service.RadarReliabilityFactsRepository)
	if !ok {
		response.Error(c, http.StatusServiceUnavailable, "Radar reliability facts reader is not available")
		return
	}
	facts, err := reader.GetReliabilityFacts(c.Request.Context(), runID, policyID, profileID)
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "Reliability facts not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, facts)
}

func (h *RadarGovernanceHandler) ActivateReleaseSubject(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionGateDecide)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		EffectiveAt time.Time `json:"effective_at" binding:"required"`
		ExpiresAt   time.Time `json:"expires_at" binding:"required"`
	}
	if !decodeJSON(c, &req) {
		return
	}
	event, err := h.repo.ActivateReleaseSubject(c.Request.Context(), service.ReleaseSubjectActivationInput{
		ReleaseSubjectID: id, ActorID: actorID, EffectiveAt: req.EffectiveAt, ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, event)
}

func (h *RadarGovernanceHandler) RevokeReleaseSubject(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionGateDecide)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	event, err := h.repo.RevokeReleaseSubject(c.Request.Context(), id, actorID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, event)
}

type radarGatePolicyRequest struct {
	Version             int             `json:"version" binding:"required,gt=0"`
	Policy              json.RawMessage `json:"policy" binding:"required"`
	EnforcementStartsAt time.Time       `json:"enforcement_starts_at" binding:"required"`
	ApprovalExpiresAt   time.Time       `json:"approval_expires_at" binding:"required"`
}

func (h *RadarGovernanceHandler) CreateGatePolicy(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionPolicyManage)
	if !ok {
		return
	}
	var req radarGatePolicyRequest
	if !decodeJSON(c, &req) {
		return
	}
	policyDocument := rawOrEmpty(req.Policy)
	policyHash, err := service.DigestCanonicalJSON(policyDocument)
	if err != nil {
		response.BadRequest(c, "gate policy must be valid canonical JSON")
		return
	}
	policy, err := h.repo.CreateGatePolicy(c.Request.Context(), service.RadarGatePolicyInput{
		Version: req.Version, Policy: policyDocument, PolicyHash: policyHash,
		EnforcementStartsAt: req.EnforcementStartsAt, ApprovalExpiresAt: req.ApprovalExpiresAt, CreatedBy: actorID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, policy)
}

type radarGatePolicyApprovalRequest struct {
	Role         service.RadarRole `json:"role" binding:"required"`
	PolicyHash   string            `json:"policy_hash" binding:"required,len=64"`
	EvidenceHash string            `json:"evidence_hash" binding:"required,len=64"`
	EffectiveAt  time.Time         `json:"effective_at" binding:"required"`
	ExpiresAt    time.Time         `json:"expires_at" binding:"required"`
}

func (h *RadarGovernanceHandler) ApproveGatePolicy(c *gin.Context) {
	actorID, ok := h.actor(c)
	if !ok || h == nil || h.repo == nil {
		return
	}
	var req radarGatePolicyApprovalRequest
	if !decodeJSON(c, &req) {
		return
	}
	if req.Role != service.RoleQualityAdmin && req.Role != service.RoleReleaseManager {
		response.BadRequest(c, "Approval role must be quality_admin or release_manager")
		return
	}
	if err := h.repo.Require(c.Request.Context(), actorID, service.PermissionPolicyApprove); err != nil {
		if errors.Is(err, service.ErrRadarForbidden) {
			response.Forbidden(c, "Radar permission denied")
		} else {
			response.ErrorFrom(c, err)
		}
		return
	}
	policyID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	approvalRepo, ok := h.repo.(service.RadarGatePolicyApprovalRepository)
	if !ok {
		response.Error(c, http.StatusServiceUnavailable, "Radar gate policy approval is not available")
		return
	}
	approval, err := approvalRepo.ApproveGatePolicy(c.Request.Context(), service.RadarGatePolicyApprovalInput{
		PolicyID: policyID, ApproverID: actorID, Role: req.Role, PolicyHash: req.PolicyHash,
		EvidenceHash: req.EvidenceHash, EffectiveAt: req.EffectiveAt, ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, approval)
}

func (h *RadarGovernanceHandler) ActivateGatePolicy(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionPolicyManage)
	if !ok {
		return
	}
	policyID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Environment      string     `json:"environment" binding:"required"`
		ScopeType        string     `json:"scope_type" binding:"required"`
		ScopeID          string     `json:"scope_id" binding:"required"`
		ExpectedPolicyID *uuid.UUID `json:"expected_policy_id"`
	}
	if !decodeJSON(c, &req) {
		return
	}
	head, err := h.repo.ActivateGatePolicy(c.Request.Context(), service.RadarGatePolicyActivationInput{
		PolicyID: policyID,
		Scope: service.RadarGovernanceScope{
			Environment: req.Environment,
			ScopeType:   req.ScopeType,
			ScopeID:     req.ScopeID,
		},
		ActorID: actorID, ExpectedPolicyID: req.ExpectedPolicyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, head)
}

type radarGateEvaluationRequest struct {
	RunID      uuid.UUID  `json:"run_id" binding:"required"`
	BaselineID *uuid.UUID `json:"baseline_id"`
	PolicyID   uuid.UUID  `json:"policy_id" binding:"required"`
}

func (h *RadarGovernanceHandler) EvaluateGate(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionGateDecide); !ok {
		return
	}
	var req radarGateEvaluationRequest
	if !decodeJSON(c, &req) {
		return
	}
	loader, ok := h.repo.(service.RadarGateReliabilityLoader)
	if !ok {
		response.Error(c, http.StatusServiceUnavailable, "Radar reliability evidence loader is not available")
		return
	}
	reliability, err := loader.LoadRadarGateReliability(c.Request.Context(), req.RunID, req.PolicyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if reliability == nil {
		response.Error(c, http.StatusServiceUnavailable, "Radar reliability evidence is not available")
		return
	}
	if !reliability.InputLoaded {
		response.Error(c, http.StatusServiceUnavailable, "Radar gate input is not available from the trusted evidence loader")
		return
	}
	input := reliability.Input
	input.ObservedAt = reliability.ObservedAt
	input.Reliability = &reliability.Evidence
	decision := service.EvaluateRadarGate(reliability.Policy, input)
	evidence, evidenceHash, err := service.BuildRadarGateEvidenceEnvelope(
		req.RunID, req.PolicyID, reliability.PolicyHash, reliability.ObservedAt, input, reliability.Evidence,
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	record, err := h.repo.RecordGateDecision(c.Request.Context(), service.RadarGateDecisionInput{
		RunID: req.RunID, BaselineID: req.BaselineID, PolicyID: req.PolicyID,
		Status: decision.Status, RuleIDs: []string{decision.RuleID},
		Evidence: evidence, EvidenceHash: evidenceHash,
		ReleaseSubjectHash: reliability.ReleaseSubjectHash, SourceWatermark: reliability.SourceWatermark,
		SupersedesDecisionID: reliability.SupersedesDecisionID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, record)
}

func (h *RadarGovernanceHandler) RecordGateDecision(c *gin.Context) {
	// Gate decisions must be produced by EvaluateGate after loading a repeatable
	// read-only evidence snapshot. Keep this method as an explicit conflict for
	// old callers so no request can write an arbitrary status or evidence.
	response.Error(c, http.StatusConflict, "direct gate decision recording is disabled; use /gates/evaluate")
}

type radarGateWaiverRequest struct {
	DecisionID      uuid.UUID `json:"decision_id" binding:"required"`
	BusinessReason  string    `json:"business_reason" binding:"required"`
	RiskOwnerUserID int64     `json:"risk_owner_user_id" binding:"required,gt=0"`
	Mitigation      string    `json:"mitigation" binding:"required"`
	RetestPlan      string    `json:"retest_plan" binding:"required"`
	ExpiresAt       time.Time `json:"expires_at" binding:"required"`
}

func (h *RadarGovernanceHandler) WaiveGateDecision(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionGateWaive)
	if !ok {
		return
	}
	var req radarGateWaiverRequest
	if !decodeJSON(c, &req) {
		return
	}
	if !req.ExpiresAt.After(time.Now().UTC()) {
		response.BadRequest(c, "expires_at must be in the future")
		return
	}
	waiver, err := h.repo.WaiveGateDecision(c.Request.Context(), service.RadarGateWaiverInput{
		DecisionID: req.DecisionID, BusinessReason: strings.TrimSpace(req.BusinessReason),
		RiskOwnerUserID: req.RiskOwnerUserID, Mitigation: strings.TrimSpace(req.Mitigation),
		RetestPlan: strings.TrimSpace(req.RetestPlan), ExpiresAt: req.ExpiresAt, ApprovedBy: actorID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, waiver)
}

type radarAlertObservationRequest struct {
	ModelRoute       string                  `json:"model_route" binding:"required"`
	CapabilityDomain string                  `json:"capability_domain" binding:"required"`
	Cause            service.RadarAlertCause `json:"cause" binding:"required"`
	PolicyVersion    int                     `json:"policy_version" binding:"required,gt=0"`
	Severity         string                  `json:"severity" binding:"required"`
	Confidence       *float64                `json:"confidence"`
	ObservedAt       time.Time               `json:"observed_at"`
	Payload          json.RawMessage         `json:"payload"`
}

func (h *RadarGovernanceHandler) ObserveAlert(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionGateDecide); !ok {
		return
	}
	var req radarAlertObservationRequest
	if !decodeJSON(c, &req) {
		return
	}
	alert, err := h.repo.ObserveAlert(c.Request.Context(), service.RadarAlertObservationInput{
		ModelRoute: req.ModelRoute, CapabilityDomain: req.CapabilityDomain, Cause: req.Cause,
		PolicyVersion: req.PolicyVersion, Severity: req.Severity, Confidence: req.Confidence,
		ObservedAt: req.ObservedAt, Payload: rawOrEmpty(req.Payload),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, alert)
}

func (h *RadarGovernanceHandler) GetAlert(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionView); !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	alert, err := h.repo.GetAlert(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "Alert not found")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, alert)
}

func (h *RadarGovernanceHandler) AcknowledgeAlert(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionGateDecide)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	if err := h.repo.AcknowledgeAlert(c.Request.Context(), id, actorID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"id": id, "status": service.RadarAlertStatusAcknowledged})
}

type radarRecoveryRequest struct {
	RecoveryTestID uuid.UUID       `json:"recovery_test_id" binding:"required"`
	Passed         bool            `json:"passed"`
	Payload        json.RawMessage `json:"payload"`
}

func (h *RadarGovernanceHandler) RecordAlertRecovery(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionGateDecide)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req radarRecoveryRequest
	if !decodeJSON(c, &req) {
		return
	}
	if err := h.repo.RecordAlertRecovery(c.Request.Context(), id, req.RecoveryTestID, req.Passed, actorID, rawOrEmpty(req.Payload)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"id": id, "recovery_test_id": req.RecoveryTestID, "passed": req.Passed})
}

func (h *RadarGovernanceHandler) ResolveAlert(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionGateDecide)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	if err := h.repo.ResolveAlert(c.Request.Context(), id, actorID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"id": id, "status": service.RadarAlertStatusResolved})
}

type radarAttributionRequest struct {
	Cause        service.RadarAlertCause `json:"cause" binding:"required"`
	Confidence   *float64                `json:"confidence"`
	RouteSlices  json.RawMessage         `json:"route_slices"`
	EvidenceHash string                  `json:"evidence_hash" binding:"required,len=64"`
}

func (h *RadarGovernanceHandler) RecordAttribution(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionGateDecide); !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req radarAttributionRequest
	if !decodeJSON(c, &req) {
		return
	}
	attribution, err := h.repo.RecordAttribution(c.Request.Context(), service.RadarAttributionInput{
		AlertID: id, Cause: req.Cause, Confidence: req.Confidence,
		RouteSlices: rawOrEmpty(req.RouteSlices), EvidenceHash: req.EvidenceHash,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, attribution)
}

// The current governance repository intentionally exposes writes and point
// lookups. These projections keep the console contract stable until the report
// repository is available, while avoiding raw route/account identifiers.
func (h *RadarGovernanceHandler) Overview(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionView); !ok {
		return
	}
	projection, ok := h.repo.(service.RadarProjectionRepository)
	if !ok {
		response.Success(c, gin.H{"freshness": time.Now().UTC(), "models": []any{}, "alerts": []any{}, "gates": []any{}, "workers": []any{}, "summary": gin.H{"models": 0, "open_alerts": 0, "blocked_gates": 0, "healthy_workers": 0}})
		return
	}
	models, err := projection.ListModelHealth(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	alerts, err := projection.ListAlerts(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	gates, err := projection.ListGates(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	workers, err := projection.ListWorkers(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	healthyWorkers := 0
	openAlerts := 0
	blockedGates := 0
	for _, worker := range workers {
		if worker.Status == "active" {
			healthyWorkers++
		}
	}
	for _, alert := range alerts {
		if alert.Status != service.RadarAlertStatusResolved {
			openAlerts++
		}
	}
	for _, gate := range gates {
		if gate.Status == service.RadarGateBlocked {
			blockedGates++
		}
	}
	response.Success(c, gin.H{"freshness": time.Now().UTC(), "models": models, "alerts": alerts, "gates": gates, "workers": workers, "summary": gin.H{"models": len(models), "open_alerts": openAlerts, "blocked_gates": blockedGates, "healthy_workers": healthyWorkers}})
}

func (h *RadarGovernanceHandler) EmptyModels(c *gin.Context) {
	h.projectionList(c, func(p service.RadarProjectionRepository, ctx context.Context) (any, error) {
		return p.ListModelHealth(ctx)
	})
}
func (h *RadarGovernanceHandler) EmptyRuns(c *gin.Context) {
	h.projectionList(c, func(p service.RadarProjectionRepository, ctx context.Context) (any, error) { return p.ListRuns(ctx) })
}
func (h *RadarGovernanceHandler) EmptyAlerts(c *gin.Context) {
	h.projectionList(c, func(p service.RadarProjectionRepository, ctx context.Context) (any, error) { return p.ListAlerts(ctx) })
}
func (h *RadarGovernanceHandler) EmptyGates(c *gin.Context) {
	h.projectionList(c, func(p service.RadarProjectionRepository, ctx context.Context) (any, error) { return p.ListGates(ctx) })
}
func (h *RadarGovernanceHandler) EmptyWorkers(c *gin.Context) {
	h.projectionList(c, func(p service.RadarProjectionRepository, ctx context.Context) (any, error) { return p.ListWorkers(ctx) })
}
func (h *RadarGovernanceHandler) EmptyDatasets(c *gin.Context) {
	h.projectionList(c, func(p service.RadarProjectionRepository, ctx context.Context) (any, error) {
		return p.ListDatasets(ctx)
	})
}

type radarWorkerRegistrationRequest struct {
	Name           string   `json:"name" binding:"required"`
	WorkerKind     string   `json:"worker_kind" binding:"required"`
	Region         string   `json:"region"`
	ImageDigest    string   `json:"image_digest"`
	Capabilities   []string `json:"capabilities"`
	MaxConcurrency int      `json:"max_concurrency"`
	Token          string   `json:"token" binding:"required"`
}

type radarWorkerTokenRequest struct {
	Token string `json:"token" binding:"required"`
}

func (h *RadarGovernanceHandler) workerRepo(c *gin.Context) (service.RadarWorkerRepository, int64, bool) {
	actorID, ok := h.require(c, service.PermissionWorkerManage)
	if !ok {
		return nil, 0, false
	}
	repo, ok := h.repo.(service.RadarWorkerRepository)
	if !ok || repo == nil {
		response.Error(c, http.StatusServiceUnavailable, "Radar worker control is not available")
		return nil, 0, false
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(key) != 64 {
		response.BadRequest(c, "Idempotency-Key must be 64 characters")
		return nil, 0, false
	}
	return repo, actorID, true
}

func (h *RadarGovernanceHandler) RegisterWorker(c *gin.Context) {
	repo, actorID, ok := h.workerRepo(c)
	if !ok {
		return
	}
	var req radarWorkerRegistrationRequest
	if !decodeJSON(c, &req) {
		return
	}
	worker, err := repo.RegisterRadarWorker(c.Request.Context(), service.RadarWorkerRegistrationInput{Name: req.Name, WorkerKind: req.WorkerKind, Region: req.Region, ImageDigest: req.ImageDigest, Capabilities: req.Capabilities, MaxConcurrency: req.MaxConcurrency, Token: req.Token}, actorID, strings.TrimSpace(c.GetHeader("Idempotency-Key")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, worker)
}

func (h *RadarGovernanceHandler) RotateWorkerToken(c *gin.Context) {
	repo, actorID, ok := h.workerRepo(c)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req radarWorkerTokenRequest
	if !decodeJSON(c, &req) {
		return
	}
	worker, err := repo.RotateRadarWorkerToken(c.Request.Context(), id, req.Token, actorID, strings.TrimSpace(c.GetHeader("Idempotency-Key")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, worker)
}

func (h *RadarGovernanceHandler) setWorkerMode(c *gin.Context, mode service.WorkerClaimMode) {
	repo, actorID, ok := h.workerRepo(c)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	worker, err := repo.SetRadarWorkerClaimMode(c.Request.Context(), id, mode, actorID, strings.TrimSpace(c.GetHeader("Idempotency-Key")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, worker)
}

func (h *RadarGovernanceHandler) PauseWorkerClaims(c *gin.Context) {
	h.setWorkerMode(c, service.WorkerClaimsPaused)
}
func (h *RadarGovernanceHandler) ResumeWorkerClaims(c *gin.Context) {
	h.setWorkerMode(c, service.WorkerClaimsOpen)
}
func (h *RadarGovernanceHandler) DrainWorker(c *gin.Context) {
	h.setWorkerMode(c, service.WorkerClaimsDraining)
}

func (h *RadarGovernanceHandler) DisableWorker(c *gin.Context) {
	repo, actorID, ok := h.workerRepo(c)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	worker, err := repo.DisableRadarWorker(c.Request.Context(), id, actorID, strings.TrimSpace(c.GetHeader("Idempotency-Key")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, worker)
}

type radarRunControlRequest struct {
	Reason string `json:"reason" binding:"required"`
}

func (h *RadarGovernanceHandler) runControl(c *gin.Context, action string) {
	actorID, ok := h.require(c, service.PermissionRunControl)
	if !ok {
		return
	}
	repo, ok := h.repo.(service.RunControlRepository)
	if !ok {
		response.Error(c, http.StatusServiceUnavailable, "Radar run control is not available")
		return
	}
	runID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(key) != 64 {
		response.BadRequest(c, "Idempotency-Key must be 64 characters")
		return
	}
	var req radarRunControlRequest
	if !decodeJSON(c, &req) {
		return
	}
	var result *service.RunControlResult
	var err error
	switch action {
	case "pause":
		result, err = repo.PauseRun(c.Request.Context(), runID, req.Reason, actorID, key)
	case "resume":
		result, err = repo.ResumeRun(c.Request.Context(), runID, req.Reason, actorID, key)
	case "cancel":
		result, err = repo.CancelRun(c.Request.Context(), runID, req.Reason, actorID, key)
	case "fence":
		result, err = repo.FenceRun(c.Request.Context(), runID, req.Reason, actorID, key)
	default:
		response.BadRequest(c, "Invalid run control action")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *RadarGovernanceHandler) PauseRun(c *gin.Context)  { h.runControl(c, "pause") }
func (h *RadarGovernanceHandler) ResumeRun(c *gin.Context) { h.runControl(c, "resume") }
func (h *RadarGovernanceHandler) CancelRun(c *gin.Context) { h.runControl(c, "cancel") }
func (h *RadarGovernanceHandler) FenceRun(c *gin.Context)  { h.runControl(c, "fence") }

type radarRevisionBatchRequest struct {
	RunID  uuid.UUID `json:"run_id" binding:"required"`
	Reason string    `json:"reason" binding:"required"`
}

type radarRevisionBatchControlRequest struct {
	Reason string `json:"reason" binding:"required"`
}

type radarCompensatingHeadRequest struct {
	SampleID uuid.UUID        `json:"sample_id" binding:"required"`
	GraderID string           `json:"grader_id" binding:"required"`
	ScoreRef service.ScoreRef `json:"score_ref" binding:"required"`
}

func (h *RadarGovernanceHandler) revisionBatchRepo(c *gin.Context, permission service.RadarPermission) (service.RevisionBatchRepository, int64, string, bool) {
	actorID, ok := h.require(c, permission)
	if !ok {
		return nil, 0, "", false
	}
	repo, ok := h.repo.(service.RevisionBatchRepository)
	if !ok || repo == nil {
		response.Error(c, http.StatusServiceUnavailable, "Radar revision control is not available")
		return nil, 0, "", false
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if err := service.ValidateRevisionBatchIdempotencyKey(key); err != nil {
		response.BadRequest(c, "Idempotency-Key must be 64 lowercase hexadecimal characters")
		return nil, 0, "", false
	}
	return repo, actorID, key, true
}

func respondRevisionBatchError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrRevisionBatchInvalid):
		response.BadRequest(c, err.Error())
	case errors.Is(err, service.ErrRevisionBatchRunNotCompleted),
		errors.Is(err, service.ErrRevisionBatchConflict),
		errors.Is(err, service.ErrRevisionBatchFenced),
		errors.Is(err, service.ErrRevisionBatchPropagationRequired),
		errors.Is(err, service.ErrRevisionBatchNotRepairable):
		response.Error(c, http.StatusConflict, err.Error())
	default:
		response.ErrorFrom(c, err)
	}
}

func (h *RadarGovernanceHandler) CreateRevisionBatch(c *gin.Context) {
	repo, actorID, key, ok := h.revisionBatchRepo(c, service.PermissionRunRetry)
	if !ok {
		return
	}
	var req radarRevisionBatchRequest
	if !decodeJSON(c, &req) {
		return
	}
	batch, err := repo.CreateRevisionBatch(c.Request.Context(), service.CreateRevisionBatchInput{
		RunID: req.RunID, Reason: req.Reason, RequestedBy: actorID, IdempotencyKey: key,
	})
	if err != nil {
		respondRevisionBatchError(c, err)
		return
	}
	response.Created(c, batch)
}

func (h *RadarGovernanceHandler) revisionBatchControl(c *gin.Context, action string) {
	permission := service.PermissionRunControl
	if action == "repair" {
		permission = service.PermissionRunRetry
	}
	repo, actorID, key, ok := h.revisionBatchRepo(c, permission)
	if !ok {
		return
	}
	batchID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req radarRevisionBatchControlRequest
	if !decodeJSON(c, &req) {
		return
	}
	input := service.RevisionBatchControlInput{
		BatchID: batchID, Reason: req.Reason, ActorID: actorID, IdempotencyKey: key,
	}
	var batch *service.RevisionBatch
	var err error
	switch action {
	case "fence":
		batch, err = repo.FenceRevisionBatch(c.Request.Context(), input)
	case "resume":
		batch, err = repo.ResumeRevisionBatch(c.Request.Context(), input)
	case "cancel":
		batch, err = repo.CancelRevisionBatch(c.Request.Context(), input)
	case "repair":
		batch, err = repo.RepairRevisionBatch(c.Request.Context(), input)
	default:
		response.BadRequest(c, "Invalid revision batch action")
		return
	}
	if err != nil {
		respondRevisionBatchError(c, err)
		return
	}
	response.Success(c, batch)
}

func (h *RadarGovernanceHandler) FenceRevisionBatch(c *gin.Context) {
	h.revisionBatchControl(c, "fence")
}
func (h *RadarGovernanceHandler) ResumeRevisionBatch(c *gin.Context) {
	h.revisionBatchControl(c, "resume")
}
func (h *RadarGovernanceHandler) CancelRevisionBatch(c *gin.Context) {
	h.revisionBatchControl(c, "cancel")
}
func (h *RadarGovernanceHandler) RepairRevisionBatch(c *gin.Context) {
	h.revisionBatchControl(c, "repair")
}

func (h *RadarGovernanceHandler) ApproveCompensatingScoreHead(c *gin.Context) {
	repo, actorID, key, ok := h.revisionBatchRepo(c, service.PermissionBaselineQualityApprove)
	if !ok {
		return
	}
	batchID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req radarCompensatingHeadRequest
	if !decodeJSON(c, &req) {
		return
	}
	result, err := repo.ApproveCompensatingScoreHead(c.Request.Context(), service.CompensatingScoreHeadInput{
		BatchID: batchID, SampleID: req.SampleID, GraderID: req.GraderID,
		ScoreRef: req.ScoreRef, ActorID: actorID, IdempotencyKey: key,
	})
	if err != nil {
		respondRevisionBatchError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *RadarGovernanceHandler) projectionList(c *gin.Context, load func(service.RadarProjectionRepository, context.Context) (any, error)) {
	if _, ok := h.require(c, service.PermissionView); !ok {
		return
	}
	projection, ok := h.repo.(service.RadarProjectionRepository)
	if !ok {
		response.Success(c, []any{})
		return
	}
	items, err := load(projection, c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}
