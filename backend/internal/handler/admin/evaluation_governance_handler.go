package admin

import (
	"context"
	"database/sql"
	"encoding/hex"
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
// The authenticated subject is the only source of actor identity.
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

type radarWorkerRegistrationRequest struct {
	Name           string   `json:"name" binding:"required"`
	WorkerKind     string   `json:"worker_kind" binding:"required"`
	Region         string   `json:"region" binding:"required"`
	ImageDigest    string   `json:"image_digest" binding:"required"`
	Capabilities   []string `json:"capabilities"`
	MaxConcurrency int      `json:"max_concurrency" binding:"required,gt=0,lte=1000"`
	Token          string   `json:"token" binding:"required"`
	IdempotencyKey string   `json:"idempotency_key" binding:"required,len=64"`
}

type radarWorkerTokenRotationRequest struct {
	Token          string `json:"token" binding:"required"`
	IdempotencyKey string `json:"idempotency_key" binding:"required,len=64"`
}

type radarWorkerActionRequest struct {
	Reason         string `json:"reason" binding:"required"`
	IdempotencyKey string `json:"idempotency_key" binding:"required,len=64"`
}

func validWorkerIdempotencyKeyRequest(c *gin.Context, value string) bool {
	if len(value) == 64 {
		if _, err := hex.DecodeString(value); err == nil {
			return true
		}
	}
	response.BadRequest(c, "idempotency_key must be 64 hexadecimal characters")
	return false
}

func (h *RadarGovernanceHandler) RegisterWorker(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionWorkerManage)
	if !ok {
		return
	}
	var req radarWorkerRegistrationRequest
	if !decodeJSON(c, &req) {
		return
	}
	if !validWorkerIdempotencyKeyRequest(c, req.IdempotencyKey) {
		return
	}
	worker, err := h.repo.RegisterWorker(c.Request.Context(), service.RadarWorkerRegistrationInput{
		Name: req.Name, WorkerKind: req.WorkerKind, Region: req.Region, ImageDigest: req.ImageDigest,
		Capabilities: req.Capabilities, MaxConcurrency: req.MaxConcurrency, Token: req.Token,
		ActorID: actorID, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, worker)
}

func (h *RadarGovernanceHandler) RotateWorkerToken(c *gin.Context) {
	actorID, ok := h.require(c, service.PermissionWorkerManage)
	if !ok {
		return
	}
	workerID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req radarWorkerTokenRotationRequest
	if !decodeJSON(c, &req) {
		return
	}
	if !validWorkerIdempotencyKeyRequest(c, req.IdempotencyKey) {
		return
	}
	worker, err := h.repo.RotateWorkerToken(c.Request.Context(), service.RadarWorkerTokenRotationInput{
		WorkerID: workerID, Token: req.Token, ActorID: actorID, IdempotencyKey: req.IdempotencyKey,
	})
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "Worker not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, worker)
}

func (h *RadarGovernanceHandler) workerAction(c *gin.Context, action func(service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error)) {
	actorID, ok := h.require(c, service.PermissionWorkerManage)
	if !ok {
		return
	}
	workerID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req radarWorkerActionRequest
	if !decodeJSON(c, &req) {
		return
	}
	if !validWorkerIdempotencyKeyRequest(c, req.IdempotencyKey) {
		return
	}
	result, err := action(service.RadarWorkerActionInput{
		WorkerID: workerID, Reason: strings.TrimSpace(req.Reason), ActorID: actorID, IdempotencyKey: req.IdempotencyKey,
	})
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "Worker not found")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *RadarGovernanceHandler) PauseWorkerClaims(c *gin.Context) {
	h.workerAction(c, func(input service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error) {
		return h.repo.PauseWorkerClaims(c.Request.Context(), input)
	})
}

func (h *RadarGovernanceHandler) ResumeWorkerClaims(c *gin.Context) {
	h.workerAction(c, func(input service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error) {
		return h.repo.ResumeWorkerClaims(c.Request.Context(), input)
	})
}

func (h *RadarGovernanceHandler) DrainWorker(c *gin.Context) {
	h.workerAction(c, func(input service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error) {
		return h.repo.DrainWorker(c.Request.Context(), input)
	})
}

func (h *RadarGovernanceHandler) DisableWorker(c *gin.Context) {
	h.workerAction(c, func(input service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error) {
		return h.repo.DisableWorker(c.Request.Context(), input)
	})
}

type radarRoleBindingRequest struct {
	ActorID int64             `json:"actor_id" binding:"required,gt=0"`
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
	binding, err := h.repo.CreateRoleBinding(c.Request.Context(), service.RadarRoleBindingInput{
		ActorID: req.ActorID, Role: req.Role, Scope: rawOrEmpty(req.Scope), CreatedBy: createdBy,
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
	baseline, err := h.repo.ActivateBaseline(c.Request.Context(), id, actorID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, baseline)
}

type radarGatePolicyRequest struct {
	Version             int             `json:"version" binding:"required,gt=0"`
	Policy              json.RawMessage `json:"policy" binding:"required"`
	PolicyHash          string          `json:"policy_hash" binding:"required,len=64"`
	EnforcementStartsAt time.Time       `json:"enforcement_starts_at" binding:"required"`
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
	policy, err := h.repo.CreateGatePolicy(c.Request.Context(), service.RadarGatePolicyInput{
		Version: req.Version, Policy: rawOrEmpty(req.Policy), PolicyHash: req.PolicyHash,
		EnforcementStartsAt: req.EnforcementStartsAt, CreatedBy: actorID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, policy)
}

type radarGateDecisionRequest struct {
	RunID        uuid.UUID                       `json:"run_id" binding:"required"`
	BaselineID   *uuid.UUID                      `json:"baseline_id"`
	PolicyID     uuid.UUID                       `json:"policy_id" binding:"required"`
	Status       service.RadarGateDecisionStatus `json:"status" binding:"required"`
	RuleIDs      []string                        `json:"rule_ids"`
	Evidence     json.RawMessage                 `json:"evidence"`
	EvidenceHash string                          `json:"evidence_hash" binding:"required,len=64"`
}

type radarGateEvaluationRequest struct {
	RunID        uuid.UUID                 `json:"run_id" binding:"required"`
	BaselineID   *uuid.UUID                `json:"baseline_id"`
	PolicyID     uuid.UUID                 `json:"policy_id" binding:"required"`
	Policy       radarGateEvaluationPolicy `json:"policy" binding:"required"`
	Input        radarGateEvaluationInput  `json:"input" binding:"required"`
	Evidence     json.RawMessage           `json:"evidence"`
	EvidenceHash string                    `json:"evidence_hash" binding:"required,len=64"`
}

type radarGateEvaluationPolicy struct {
	Version               int       `json:"version" binding:"required,gt=0"`
	ObservationDays       int       `json:"observation_days" binding:"required,gte=0"`
	EnforcementStartsAt   time.Time `json:"enforcement_starts_at" binding:"required"`
	CriticalDomainDeltaPP float64   `json:"critical_domain_delta_pp"`
	AggregateDeltaPP      float64   `json:"aggregate_delta_pp"`
	ConfidenceLevel       float64   `json:"confidence_level"`
	RequireCIExcludeZero  bool      `json:"require_ci_exclude_zero"`
}

type radarGateEvaluationInput struct {
	EvidenceSufficient     bool      `json:"evidence_sufficient"`
	RouteEvidencePresent   bool      `json:"route_evidence_present"`
	RouteMatch             bool      `json:"route_match"`
	ObservedAt             time.Time `json:"observed_at" binding:"required"`
	ObservationDays        int       `json:"observation_days" binding:"gte=0"`
	NewP0Failure           bool      `json:"new_p0_failure"`
	CriticalDeltaPP        float64   `json:"critical_delta_pp"`
	CriticalCIHighPP       float64   `json:"critical_ci_high_pp"`
	AggregateDeltaPP       float64   `json:"aggregate_delta_pp"`
	AggregateCIHighPP      float64   `json:"aggregate_ci_high_pp"`
	ReliabilitySLOBreached bool      `json:"reliability_slo_breached"`
	JudgeDisagreement      bool      `json:"judge_disagreement"`
}

func (h *RadarGovernanceHandler) EvaluateGate(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionGateDecide); !ok {
		return
	}
	var req radarGateEvaluationRequest
	if !decodeJSON(c, &req) {
		return
	}
	decision := service.EvaluateRadarGate(service.RadarGatePolicy{
		Version: req.Policy.Version, ObservationDays: req.Policy.ObservationDays,
		EnforcementStartsAt:   req.Policy.EnforcementStartsAt,
		CriticalDomainDeltaPP: req.Policy.CriticalDomainDeltaPP,
		AggregateDeltaPP:      req.Policy.AggregateDeltaPP,
		ConfidenceLevel:       req.Policy.ConfidenceLevel,
		RequireCIExcludeZero:  req.Policy.RequireCIExcludeZero,
	}, service.RadarGateInput{
		EvidenceSufficient:   req.Input.EvidenceSufficient,
		RouteEvidencePresent: req.Input.RouteEvidencePresent, RouteMatch: req.Input.RouteMatch,
		ObservedAt: req.Input.ObservedAt, ObservationDays: req.Input.ObservationDays,
		NewP0Failure: req.Input.NewP0Failure, CriticalDeltaPP: req.Input.CriticalDeltaPP,
		CriticalCIHighPP: req.Input.CriticalCIHighPP, AggregateDeltaPP: req.Input.AggregateDeltaPP,
		AggregateCIHighPP:      req.Input.AggregateCIHighPP,
		ReliabilitySLOBreached: req.Input.ReliabilitySLOBreached,
		JudgeDisagreement:      req.Input.JudgeDisagreement,
	})
	record, err := h.repo.RecordGateDecision(c.Request.Context(), service.RadarGateDecisionInput{
		RunID: req.RunID, BaselineID: req.BaselineID, PolicyID: req.PolicyID,
		Status: decision.Status, RuleIDs: []string{decision.RuleID},
		Evidence: rawOrEmpty(req.Evidence), EvidenceHash: req.EvidenceHash,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, record)
}

func (h *RadarGovernanceHandler) RecordGateDecision(c *gin.Context) {
	if _, ok := h.require(c, service.PermissionGateDecide); !ok {
		return
	}
	var req radarGateDecisionRequest
	if !decodeJSON(c, &req) {
		return
	}
	decision, err := h.repo.RecordGateDecision(c.Request.Context(), service.RadarGateDecisionInput{
		RunID: req.RunID, BaselineID: req.BaselineID, PolicyID: req.PolicyID, Status: req.Status,
		RuleIDs: req.RuleIDs, Evidence: rawOrEmpty(req.Evidence), EvidenceHash: req.EvidenceHash,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, decision)
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

func (h *RadarGovernanceHandler) emptyList(c *gin.Context, permission service.RadarPermission) {
	if _, ok := h.require(c, permission); !ok {
		return
	}
	response.Success(c, []any{})
}
