package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrGovernanceHeadConflict = errors.New("evaluation governance head changed")
	ErrTrackedModelNotFound   = errors.New("tracked model not found")
)

// RadarGovernanceRepository is the durable control-plane contract for Radar
// governance. Implementations must keep decisions and alert transitions
// append-only where the schema provides an event table, and make retries
// idempotent on the natural keys defined by the migration.
type RadarGovernanceRepository interface {
	RadarAuthorizer
	EnableEvaluationKey(ctx context.Context, keyID, actorID int64) (*RadarEvaluationKeyRecord, error)
	CreateDataset(ctx context.Context, input CreateRadarDatasetInput) (*RadarDatasetRecord, error)
	PublishDataset(ctx context.Context, datasetID uuid.UUID, actorID int64) (*RadarDatasetRecord, error)
	CreatePlan(ctx context.Context, input CreateRadarPlanInput) (*RadarPlanRecord, error)
	CreateRunWithMatrix(ctx context.Context, input CreateRunInput) (*EvaluationRun, error)

	CreateRoleBinding(ctx context.Context, input RadarRoleBindingInput) (*RadarRoleBinding, error)
	DisableRoleBinding(ctx context.Context, id uuid.UUID, actorID int64) error
	ListRoleBindings(ctx context.Context, actorID *int64) ([]RadarRoleBinding, error)

	ProposeBaseline(ctx context.Context, input RadarBaselineInput) (*RadarBaseline, error)
	ApproveBaseline(ctx context.Context, input RadarBaselineApprovalInput) (*RadarBaselineApproval, error)
	ActivateBaseline(ctx context.Context, baselineID uuid.UUID, actorID int64) (*RadarBaseline, error)
	GetBaseline(ctx context.Context, id uuid.UUID) (*RadarBaseline, error)

	CreateGatePolicy(ctx context.Context, input RadarGatePolicyInput) (*RadarGatePolicyRecord, error)
	CreateReleaseSubject(ctx context.Context, input ReleaseSubjectInput) (*ReleaseSubjectRecord, error)
	ActivateReleaseSubject(ctx context.Context, input ReleaseSubjectActivationInput) (*ReleaseSubjectEvent, error)
	RevokeReleaseSubject(ctx context.Context, subjectID uuid.UUID, actorID int64) (*ReleaseSubjectEvent, error)
	ActivateGatePolicy(ctx context.Context, input RadarGatePolicyActivationInput) (*RadarGatePolicyHead, error)
	ActivateBaselineHead(ctx context.Context, input RadarBaselineActivationInput) (*RadarBaselineHead, error)
	RecordGateDecision(ctx context.Context, input RadarGateDecisionInput) (*RadarGateDecisionRecord, error)
	WaiveGateDecision(ctx context.Context, input RadarGateWaiverInput) (*RadarGateWaiverRecord, error)
	RotateEvidenceSigningKey(ctx context.Context, input RotateEvidenceSigningKeyInput) (*EvidenceSigningKeyRecord, error)
	TransitionEvidenceSigningKey(ctx context.Context, input TransitionEvidenceSigningKeyInput) (*EvidenceSigningKeyRecord, error)

	ObserveAlert(ctx context.Context, input RadarAlertObservationInput) (*RadarAlertRecord, error)
	AcknowledgeAlert(ctx context.Context, alertID uuid.UUID, actorID int64) error
	RecordAlertRecovery(ctx context.Context, alertID uuid.UUID, recoveryTestID uuid.UUID, passed bool, actorID int64, payload json.RawMessage) error
	ResolveAlert(ctx context.Context, alertID uuid.UUID, actorID int64) error
	RecordAttribution(ctx context.Context, input RadarAttributionInput) (*RadarAttributionRecord, error)
	GetAlert(ctx context.Context, id uuid.UUID) (*RadarAlertRecord, error)
}

// RadarGatePolicyApprovalRepository is kept separate from the broad governance
// repository so read-only and test doubles do not gain an approval mutation by
// accident. Production governance repositories must implement it.
type RadarGatePolicyApprovalRepository interface {
	ApproveGatePolicy(ctx context.Context, input RadarGatePolicyApprovalInput) (*RadarGatePolicyApprovalRecord, error)
}

type RadarReleaseSubjectRepository interface {
	GetReleaseSubject(ctx context.Context, id uuid.UUID) (*ReleaseSubjectRecord, error)
}

type WorkerClaimMode string

const (
	WorkerClaimsOpen     WorkerClaimMode = "open"
	WorkerClaimsPaused   WorkerClaimMode = "paused"
	WorkerClaimsDraining WorkerClaimMode = "draining"
)

type RadarWorkerRegistrationInput struct {
	Name           string
	WorkerKind     string
	Region         string
	ImageDigest    string
	Capabilities   []string
	MaxConcurrency int
	Token          string
}

type RadarWorkerRecord struct {
	ID               uuid.UUID       `json:"id"`
	Name             string          `json:"name"`
	WorkerKind       string          `json:"worker_kind"`
	Region           string          `json:"region,omitempty"`
	ImageDigest      string          `json:"image_digest,omitempty"`
	Capabilities     []string        `json:"capabilities,omitempty"`
	MaxConcurrency   int             `json:"max_concurrency"`
	Status           string          `json:"status"`
	ClaimMode        WorkerClaimMode `json:"claim_mode"`
	TokenEpoch       int64           `json:"token_epoch"`
	TokenFingerprint string          `json:"token_fingerprint"`
	ActiveLeaseCount int             `json:"active_lease_count,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type RadarWorkerRepository interface {
	RegisterRadarWorker(ctx context.Context, input RadarWorkerRegistrationInput, actorID int64, idempotencyKey string) (*RadarWorkerRecord, error)
	RotateRadarWorkerToken(ctx context.Context, workerID uuid.UUID, token string, actorID int64, idempotencyKey string) (*RadarWorkerRecord, error)
	SetRadarWorkerClaimMode(ctx context.Context, workerID uuid.UUID, mode WorkerClaimMode, actorID int64, idempotencyKey string) (*RadarWorkerRecord, error)
	DisableRadarWorker(ctx context.Context, workerID uuid.UUID, actorID int64, idempotencyKey string) (*RadarWorkerRecord, error)
}

type RadarEvaluationKeyRecord struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	UserID       int64  `json:"user_id"`
	GroupID      *int64 `json:"group_id,omitempty"`
	IsEvaluation bool   `json:"is_evaluation"`
}

// RadarProjectionRepository exposes read-only, dashboard-safe projections.
// Implementations must omit prompts, completions, credentials and raw route
// identifiers that are not required by the console.
type RadarProjectionRepository interface {
	ListModelHealth(ctx context.Context) ([]RadarModelHealthProjection, error)
	ListRuns(ctx context.Context) ([]RadarRunProjection, error)
	ListAlerts(ctx context.Context) ([]RadarAlertProjection, error)
	ListGates(ctx context.Context) ([]RadarGateProjection, error)
	ListWorkers(ctx context.Context) ([]RadarWorkerProjection, error)
	ListDatasets(ctx context.Context) ([]RadarDatasetProjection, error)
}

type RadarTrackedModelRepository interface {
	RegisterTrackedModel(ctx context.Context, modelAlias string, actorID int64) (*RadarTrackedModel, error)
	UntrackModel(ctx context.Context, modelAlias string, actorID int64) error
}

type RadarTrackedModel struct {
	ModelAlias string    `json:"model_alias"`
	CreatedBy  int64     `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

type RadarModelHealthProjection struct {
	ModelAlias       string    `json:"model_alias,omitempty"`
	ModelRoute       string    `json:"model_route"`
	CapabilityDomain string    `json:"capability_domain,omitempty"`
	HealthState      string    `json:"health_state"`
	BaselineScore    *float64  `json:"baseline_score,omitempty"`
	CandidateScore   *float64  `json:"candidate_score,omitempty"`
	DeltaPP          *float64  `json:"delta_pp,omitempty"`
	CILowPP          *float64  `json:"ci_low_pp,omitempty"`
	CIHighPP         *float64  `json:"ci_high_pp,omitempty"`
	SampleCount      *int      `json:"sample_count,omitempty"`
	Freshness        time.Time `json:"freshness"`
	P99MS            *float64  `json:"p99_ms,omitempty"`
	ErrorRate        *float64  `json:"error_rate,omitempty"`
}

type RadarRunProjection struct {
	ID             uuid.UUID  `json:"id"`
	PlanID         uuid.UUID  `json:"plan_id"`
	TriggerSource  string     `json:"trigger_source"`
	Status         string     `json:"status"`
	ContractStatus string     `json:"contract_status"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

type RadarAlertProjection struct {
	ID                    uuid.UUID        `json:"id"`
	ModelRoute            string           `json:"model_route"`
	CapabilityDomain      string           `json:"capability_domain"`
	Cause                 RadarAlertCause  `json:"cause"`
	Severity              string           `json:"severity"`
	Status                RadarAlertStatus `json:"status"`
	AttributionConfidence *float64         `json:"attribution_confidence,omitempty"`
	FirstSeenAt           time.Time        `json:"first_seen_at"`
}

type RadarGateProjection struct {
	ID        uuid.UUID               `json:"id"`
	RunID     uuid.UUID               `json:"run_id"`
	Status    RadarGateDecisionStatus `json:"status"`
	RuleID    string                  `json:"rule_id,omitempty"`
	CreatedAt time.Time               `json:"created_at"`
}

type RadarWorkerProjection struct {
	ID              uuid.UUID  `json:"id"`
	Name            string     `json:"name"`
	WorkerKind      string     `json:"worker_kind"`
	Status          string     `json:"status"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	Capabilities    []string   `json:"capabilities,omitempty"`
}

type RadarDatasetProjection struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	Status     string    `json:"status"`
	Cases      int       `json:"cases"`
	SourceType string    `json:"source_type"`
	CreatedBy  int64     `json:"created_by"`
	TenantID   int64     `json:"tenant_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type RadarRoleBindingInput struct {
	ActorID   int64
	Role      RadarRole
	Scope     json.RawMessage
	CreatedBy int64
}

type RadarRoleBinding struct {
	ID         uuid.UUID
	ActorID    int64
	Role       RadarRole
	Scope      json.RawMessage
	Enabled    bool
	CreatedBy  *int64
	CreatedAt  time.Time
	DisabledAt *time.Time
}

type RadarBaselineInput struct {
	ModelRoute            string
	RunID                 uuid.UUID
	DatasetManifestSHA256 string
	EvidenceHash          string
	RouteProfileVersion   string
	PolicyVersion         int
	ProposedBy            int64
}

type RadarBaseline struct {
	ID                    uuid.UUID
	ModelRoute            string
	RunID                 uuid.UUID
	DatasetManifestSHA256 string
	EvidenceHash          string
	RouteProfileVersion   string
	PolicyVersion         int
	Status                string
	ProposedBy            int64
	ProposedAt            time.Time
	ActivatedAt           *time.Time
	RetiredAt             *time.Time
}

type RadarBaselineApprovalInput struct {
	BaselineID   uuid.UUID
	ApproverID   int64
	Role         RadarRole
	EvidenceHash string
	EffectiveAt  time.Time
	ExpiresAt    time.Time
}

type RadarBaselineApproval struct {
	ID           uuid.UUID
	BaselineID   uuid.UUID
	ApproverID   int64
	Role         RadarRole
	EvidenceHash string
	EffectiveAt  time.Time
	ExpiresAt    time.Time
	CreatedAt    time.Time
}

type RadarGatePolicyInput struct {
	Version             int
	Policy              json.RawMessage
	PolicyHash          string
	EnforcementStartsAt time.Time
	ApprovalExpiresAt   time.Time
	CreatedBy           int64
}

type RadarGatePolicyRecord struct {
	ID                  uuid.UUID
	Version             int
	Policy              json.RawMessage
	PolicyHash          string
	EnforcementStartsAt time.Time
	CreatedBy           int64
	CreatedAt           time.Time
	RetiredAt           *time.Time
}

type RadarGatePolicyApprovalInput struct {
	PolicyID     uuid.UUID
	ApproverID   int64
	Role         RadarRole
	PolicyHash   string
	EvidenceHash string
	EffectiveAt  time.Time
	ExpiresAt    time.Time
}

type RadarGatePolicyApprovalRecord struct {
	ID           uuid.UUID `json:"id"`
	PolicyID     uuid.UUID `json:"policy_id"`
	ApproverID   int64     `json:"approver_id"`
	Role         RadarRole `json:"role"`
	PolicyHash   string    `json:"policy_hash"`
	EvidenceHash string    `json:"evidence_hash"`
	EffectiveAt  time.Time `json:"effective_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
}

type ReleaseSubjectInput struct {
	RunID        uuid.UUID
	Subject      ReleaseSubject
	ExpectedHash string
}

type ReleaseSubjectRecord struct {
	ID          uuid.UUID      `json:"id"`
	RunID       uuid.UUID      `json:"run_id"`
	SubjectHash string         `json:"subject_hash"`
	Subject     ReleaseSubject `json:"subject"`
	CreatedAt   time.Time      `json:"created_at"`
	Active      bool           `json:"active"`
	EffectiveAt *time.Time     `json:"effective_at,omitempty"`
	ExpiresAt   *time.Time     `json:"expires_at,omitempty"`
}

type ReleaseSubjectActivationInput struct {
	ReleaseSubjectID uuid.UUID
	ActorID          int64
	EffectiveAt      time.Time
	ExpiresAt        time.Time
}

type ReleaseSubjectEvent struct {
	ID               uuid.UUID  `json:"id"`
	ReleaseSubjectID uuid.UUID  `json:"release_subject_id"`
	EventType        string     `json:"event_type"`
	ActorID          int64      `json:"actor_id"`
	EffectiveAt      time.Time  `json:"effective_at"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

type RadarGovernanceScope struct {
	Environment string `json:"environment"`
	ScopeType   string `json:"scope_type"`
	ScopeID     string `json:"scope_id"`
}

type RadarGatePolicyActivationInput struct {
	PolicyID         uuid.UUID
	Scope            RadarGovernanceScope
	ActorID          int64
	ExpectedPolicyID *uuid.UUID
}

type RadarGatePolicyHead struct {
	Scope      RadarGovernanceScope `json:"scope"`
	PolicyID   uuid.UUID            `json:"policy_id"`
	PolicyHash string               `json:"policy_hash"`
	EventID    uuid.UUID            `json:"event_id"`
	UpdatedAt  time.Time            `json:"updated_at"`
}

type RadarBaselineActivationInput struct {
	BaselineID         uuid.UUID
	Scope              RadarGovernanceScope
	ActorID            int64
	ExpectedBaselineID *uuid.UUID
}

type RadarBaselineHead struct {
	Scope      RadarGovernanceScope `json:"scope"`
	ModelRoute string               `json:"model_route"`
	BaselineID uuid.UUID            `json:"baseline_id"`
	EventID    uuid.UUID            `json:"event_id"`
	UpdatedAt  time.Time            `json:"updated_at"`
}

type RadarGateDecisionInput struct {
	RunID                uuid.UUID
	BaselineID           *uuid.UUID
	PolicyID             uuid.UUID
	Status               RadarGateDecisionStatus
	RuleIDs              []string
	Evidence             json.RawMessage
	EvidenceHash         string
	ReleaseSubjectHash   string
	SourceWatermark      json.RawMessage
	SupersedesDecisionID *uuid.UUID
	CauseSetHash         string
}

type RadarGateDecisionRecord struct {
	ID                   uuid.UUID
	RunID                uuid.UUID
	BaselineID           *uuid.UUID
	PolicyID             uuid.UUID
	Status               RadarGateDecisionStatus
	RuleIDs              []string
	Evidence             json.RawMessage
	EvidenceHash         string
	ReleaseSubjectHash   string
	SourceWatermark      json.RawMessage
	SupersedesDecisionID *uuid.UUID
	CauseSetHash         string
	CreatedAt            time.Time
}

type RadarGateWaiverInput struct {
	DecisionID      uuid.UUID
	BusinessReason  string
	RiskOwnerUserID int64
	Mitigation      string
	RetestPlan      string
	ExpiresAt       time.Time
	ApprovedBy      int64
}

type RadarGateWaiverRecord struct {
	ID              uuid.UUID
	DecisionID      uuid.UUID
	BusinessReason  string
	RiskOwnerUserID int64
	Mitigation      string
	RetestPlan      string
	ExpiresAt       time.Time
	ApprovedBy      int64
	CreatedAt       time.Time
}

type RadarAlertObservationInput struct {
	ModelRoute       string
	CapabilityDomain string
	Cause            RadarAlertCause
	PolicyVersion    int
	Severity         string
	Confidence       *float64
	ObservedAt       time.Time
	Payload          json.RawMessage
}

type RadarAlertRecord struct {
	ID                    uuid.UUID
	ModelRoute            string
	CapabilityDomain      string
	Cause                 RadarAlertCause
	PolicyVersion         int
	Status                RadarAlertStatus
	Severity              string
	AttributionConfidence *float64
	FirstSeenAt           time.Time
	AcknowledgedAt        *time.Time
	ResolvedAt            *time.Time
	RecoveryTestID        *uuid.UUID
}

type RadarAttributionInput struct {
	AlertID      uuid.UUID
	Cause        RadarAlertCause
	Confidence   *float64
	RouteSlices  json.RawMessage
	EvidenceHash string
}

type RadarAttributionRecord struct {
	ID           uuid.UUID
	AlertID      uuid.UUID
	Cause        RadarAlertCause
	Confidence   *float64
	RouteSlices  json.RawMessage
	EvidenceHash string
	CreatedAt    time.Time
}
