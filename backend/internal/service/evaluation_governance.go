package service

import (
	"context"
	"encoding/json"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

var (
	ErrRadarWorkerConflict            = infraerrors.Conflict("RADAR_WORKER_CONFLICT", "worker identity conflicts with an existing worker")
	ErrRadarWorkerIdempotencyConflict = infraerrors.Conflict("RADAR_WORKER_IDEMPOTENCY_CONFLICT", "worker idempotency key was reused with a different request")
	ErrRadarWorkerStateConflict       = infraerrors.Conflict("RADAR_WORKER_STATE_CONFLICT", "worker is not in a state that accepts this action")
	ErrRadarRunIdempotencyConflict    = infraerrors.Conflict("RADAR_RUN_IDEMPOTENCY_CONFLICT", "run idempotency key was reused with a different request")
	ErrRadarRunStateConflict          = infraerrors.Conflict("RADAR_RUN_STATE_CONFLICT", "run is not in a state that accepts this action")
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
	RegisterWorker(ctx context.Context, input RadarWorkerRegistrationInput) (*RadarWorkerRecord, error)
	RotateWorkerToken(ctx context.Context, input RadarWorkerTokenRotationInput) (*RadarWorkerRecord, error)
	PauseWorkerClaims(ctx context.Context, input RadarWorkerActionInput) (*RadarWorkerActionResult, error)
	ResumeWorkerClaims(ctx context.Context, input RadarWorkerActionInput) (*RadarWorkerActionResult, error)
	DrainWorker(ctx context.Context, input RadarWorkerActionInput) (*RadarWorkerActionResult, error)
	DisableWorker(ctx context.Context, input RadarWorkerActionInput) (*RadarWorkerActionResult, error)

	CreateRoleBinding(ctx context.Context, input RadarRoleBindingInput) (*RadarRoleBinding, error)
	DisableRoleBinding(ctx context.Context, id uuid.UUID, actorID int64) error
	ListRoleBindings(ctx context.Context, actorID *int64) ([]RadarRoleBinding, error)

	ProposeBaseline(ctx context.Context, input RadarBaselineInput) (*RadarBaseline, error)
	ApproveBaseline(ctx context.Context, input RadarBaselineApprovalInput) (*RadarBaselineApproval, error)
	ActivateBaseline(ctx context.Context, baselineID uuid.UUID, actorID int64) (*RadarBaseline, error)
	GetBaseline(ctx context.Context, id uuid.UUID) (*RadarBaseline, error)

	CreateGatePolicy(ctx context.Context, input RadarGatePolicyInput) (*RadarGatePolicyRecord, error)
	RecordGateDecision(ctx context.Context, input RadarGateDecisionInput) (*RadarGateDecisionRecord, error)
	WaiveGateDecision(ctx context.Context, input RadarGateWaiverInput) (*RadarGateWaiverRecord, error)

	ObserveAlert(ctx context.Context, input RadarAlertObservationInput) (*RadarAlertRecord, error)
	AcknowledgeAlert(ctx context.Context, alertID uuid.UUID, actorID int64) error
	RecordAlertRecovery(ctx context.Context, alertID uuid.UUID, recoveryTestID uuid.UUID, passed bool, actorID int64, payload json.RawMessage) error
	ResolveAlert(ctx context.Context, alertID uuid.UUID, actorID int64) error
	RecordAttribution(ctx context.Context, input RadarAttributionInput) (*RadarAttributionRecord, error)
	GetAlert(ctx context.Context, id uuid.UUID) (*RadarAlertRecord, error)
}

type RadarRunControlRepository interface {
	PauseRun(ctx context.Context, input RadarRunActionInput) (*RadarRunActionResult, error)
	ResumeRun(ctx context.Context, input RadarRunActionInput) (*RadarRunActionResult, error)
	CancelRun(ctx context.Context, input RadarRunActionInput) (*RadarRunActionResult, error)
	FenceRun(ctx context.Context, input RadarRunActionInput) (*RadarRunActionResult, error)
}

type RadarRunActionInput struct {
	RunID          uuid.UUID
	Reason         string
	ActorID        int64
	IdempotencyKey string
}

type RadarRunActionResult struct {
	RunID             uuid.UUID      `json:"run_id"`
	FromStatus        RunStatus      `json:"from_status"`
	ToStatus          RunStatus      `json:"to_status"`
	PreviousEpoch     int64          `json:"previous_epoch"`
	CurrentEpoch      int64          `json:"current_epoch"`
	AffectedWorkCount int            `json:"affected_work_count"`
	ReplacementIDs    []uuid.UUID    `json:"replacement_ids,omitempty"`
	EventID           uuid.UUID      `json:"event_id"`
	Idempotent        bool           `json:"idempotent"`
	Run               *EvaluationRun `json:"run,omitempty"`
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

type RadarModelHealthProjection struct {
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
	ID                    uuid.UUID  `json:"id"`
	PlanID                uuid.UUID  `json:"plan_id"`
	TriggerSource         string     `json:"trigger_source"`
	Status                string     `json:"status"`
	CreatedAt             time.Time  `json:"created_at"`
	StartedAt             *time.Time `json:"started_at,omitempty"`
	FinishedAt            *time.Time `json:"finished_at,omitempty"`
	ContractStatus        string     `json:"contract_status"`
	RequestManifestID     *uuid.UUID `json:"request_manifest_id,omitempty"`
	RequestManifestSHA256 string     `json:"request_manifest_sha256,omitempty"`
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
	ClaimMode       string     `json:"claim_mode"`
	Region          string     `json:"region,omitempty"`
	ImageDigest     string     `json:"image_digest,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	Capabilities    []string   `json:"capabilities,omitempty"`
}

type RadarWorkerRegistrationInput struct {
	Name           string
	WorkerKind     string
	Region         string
	ImageDigest    string
	Capabilities   []string
	MaxConcurrency int
	Token          string
	ActorID        int64
	IdempotencyKey string
}

type RadarWorkerTokenRotationInput struct {
	WorkerID       uuid.UUID
	Token          string
	ActorID        int64
	IdempotencyKey string
}

type RadarWorkerActionInput struct {
	WorkerID       uuid.UUID
	Reason         string
	ActorID        int64
	IdempotencyKey string
}

type RadarWorkerRecord struct {
	ID               uuid.UUID  `json:"id"`
	Name             string     `json:"name"`
	WorkerKind       string     `json:"worker_kind"`
	Region           string     `json:"region"`
	ImageDigest      string     `json:"image_digest"`
	Status           string     `json:"status"`
	ClaimMode        string     `json:"claim_mode"`
	Capabilities     []string   `json:"capabilities,omitempty"`
	MaxConcurrency   int        `json:"max_concurrency"`
	LastHeartbeatAt  *time.Time `json:"last_heartbeat_at,omitempty"`
	TokenFingerprint string     `json:"token_fingerprint"`
}

type RadarWorkerActionResult struct {
	Worker            *RadarWorkerRecord `json:"worker"`
	EventID           uuid.UUID          `json:"event_id"`
	PreviousClaimMode string             `json:"previous_claim_mode"`
	ClaimMode         string             `json:"claim_mode"`
	ActiveLeaseCount  int                `json:"active_lease_count"`
	Idempotent        bool               `json:"idempotent"`
}

type RadarDatasetProjection struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Status    string    `json:"status"`
	Cases     int       `json:"cases"`
	CreatedAt time.Time `json:"created_at"`
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
}

type RadarBaselineApproval struct {
	ID           uuid.UUID
	BaselineID   uuid.UUID
	ApproverID   int64
	Role         RadarRole
	EvidenceHash string
	CreatedAt    time.Time
}

type RadarGatePolicyInput struct {
	Version             int
	Policy              json.RawMessage
	PolicyHash          string
	EnforcementStartsAt time.Time
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

type RadarGateDecisionInput struct {
	RunID        uuid.UUID
	BaselineID   *uuid.UUID
	PolicyID     uuid.UUID
	Status       RadarGateDecisionStatus
	RuleIDs      []string
	Evidence     json.RawMessage
	EvidenceHash string
}

type RadarGateDecisionRecord struct {
	ID           uuid.UUID
	RunID        uuid.UUID
	BaselineID   *uuid.UUID
	PolicyID     uuid.UUID
	Status       RadarGateDecisionStatus
	RuleIDs      []string
	Evidence     json.RawMessage
	EvidenceHash string
	CreatedAt    time.Time
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
