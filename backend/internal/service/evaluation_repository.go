package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	ErrLeaseFenced        = errors.New("evaluation assignment lease fenced")
	ErrBudgetExceeded     = errors.New("evaluation run budget exceeded")
	ErrRadarCutoverActive = errors.New("radar cutover active")
)

const AssignmentCompleted = AssignmentStatusCompleted

type CreateRunInput struct {
	PlanID        uuid.UUID
	TriggerSource string
	BaselineRef   map[string]any
	CandidateRef  map[string]any
	CreatedBy     int64
}

type EvaluationRun struct {
	ID                    uuid.UUID       `json:"id"`
	PlanID                uuid.UUID       `json:"plan_id"`
	Status                RunStatus       `json:"status"`
	BudgetLimit           decimal.Decimal `json:"budget_limit"`
	ReservedCost          decimal.Decimal `json:"reserved_cost"`
	CreatedAt             time.Time       `json:"created_at"`
	ContractStatus        string          `json:"contract_status,omitempty"`
	RequestManifestID     uuid.UUID       `json:"request_manifest_id,omitempty"`
	RequestManifestSHA256 string          `json:"request_manifest_sha256,omitempty"`
}

type AssignmentLease struct {
	ID                     uuid.UUID           `json:"id"`
	SampleID               uuid.UUID           `json:"sample_id"`
	RunID                  uuid.UUID           `json:"run_id"`
	ModelRoute             string              `json:"model_route"`
	ModelConfig            json.RawMessage     `json:"model_config"`
	ModelConfigSHA256      string              `json:"model_config_sha256"`
	Attempt                int                 `json:"attempt"`
	Token                  string              `json:"token"`
	ExpiresAt              time.Time           `json:"expires_at"`
	Case                   *EvaluationCaseSpec `json:"case,omitempty"`
	RouteConfig            json.RawMessage     `json:"-"`
	GatewayAPIKeyID        int64               `json:"-"`
	GatewayAPIKey          string              `json:"gateway_api_key,omitempty"`
	DatasetVersion         string              `json:"dataset_version,omitempty"`
	GatewayEvaluationToken string              `json:"gateway_evaluation_token,omitempty"`
	RouteTraceID           string              `json:"route_trace_id,omitempty"`
	DatasetVersionID       uuid.UUID           `json:"dataset_version_id,omitempty"`
	DatasetKey             string              `json:"dataset_key,omitempty"`
	DatasetManifestSHA256  string              `json:"dataset_manifest_sha256,omitempty"`
}

type EvaluationCaseSpec struct {
	CaseID           uuid.UUID       `json:"case_id"`
	CaseKey          string          `json:"case_key"`
	CapabilityDomain string          `json:"capability_domain"`
	Priority         string          `json:"priority"`
	Weight           decimal.Decimal `json:"weight"`
	PromptSpec       json.RawMessage `json:"prompt_spec,omitempty"`
	ExpectedSpec     json.RawMessage `json:"expected_spec,omitempty"`
	ExecutionSpec    json.RawMessage `json:"execution_spec"`
	GraderID         string          `json:"grader_id"`
	GraderVersion    string          `json:"grader_version"`
	ContentSHA256    string          `json:"content_sha256"`
	Confidentiality  string          `json:"confidentiality"`
}

type EvidenceSubmission struct {
	AssignmentID uuid.UUID       `json:"assignment_id"`
	SampleID     uuid.UUID       `json:"sample_id"`
	Evidence     json.RawMessage `json:"evidence"`
}

type EvidenceReceipt struct {
	AssignmentID           uuid.UUID `json:"assignment_id"`
	EvidenceManifestSHA256 string    `json:"evidence_manifest_sha256"`
	AcceptedAt             time.Time `json:"accepted_at"`
}

type ArtifactPresignRequest struct {
	MIMEType string `json:"mime_type"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

type ArtifactUpload struct {
	ID        uuid.UUID `json:"artifact_id"`
	ObjectKey string    `json:"object_key"`
	UploadURL string    `json:"upload_url"`
	SHA256    string    `json:"sha256"`
	Bytes     int64     `json:"bytes"`
	MIMEType  string    `json:"mime_type"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ArtifactConfirmation struct {
	ArtifactID uuid.UUID `json:"artifact_id"`
	ObjectKey  string    `json:"object_key"`
	SHA256     string    `json:"sha256"`
	Bytes      int64     `json:"bytes"`
}

type ArtifactReceipt struct {
	ID          uuid.UUID `json:"id"`
	ObjectKey   string    `json:"object_key"`
	SHA256      string    `json:"sha256"`
	Bytes       int64     `json:"bytes"`
	MIMEType    string    `json:"mime_type"`
	ScanStatus  string    `json:"scan_status"`
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
}

type AssignmentTransition struct {
	AssignmentID uuid.UUID
	LeaseToken   string
	To           AssignmentStatus
}

type EvaluationRepository interface {
	CreateRunWithMatrix(ctx context.Context, input CreateRunInput) (*EvaluationRun, error)
	ClaimAssignment(ctx context.Context, workerID uuid.UUID, capabilities []string, leaseTTL time.Duration) (*AssignmentLease, error)
	RenewLease(ctx context.Context, assignmentID uuid.UUID, leaseToken string, extendBy time.Duration) (time.Time, error)
	TransitionAssignment(ctx context.Context, input AssignmentTransition) error
	SubmitEvidence(ctx context.Context, input EvidenceSubmission, leaseToken string) (*EvidenceReceipt, error)
}
