package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const RadarLoadPlanSchemaV1 = "radar-load-plan-v1"

var (
	ErrFaultExperimentInvalid      = errors.New("invalid radar fault experiment")
	ErrFaultActionInvalid          = errors.New("invalid radar fault action")
	ErrFaultEventInvalid           = errors.New("invalid radar fault event")
	ErrRecoveryObservationNotFound = errors.New("radar recovery observation not found")
	ErrRecoveryEvidenceInvalid     = errors.New("invalid radar recovery evidence")
)

type RadarFaultExperiment struct {
	ID            uuid.UUID  `json:"experiment_id"`
	RunID         uuid.UUID  `json:"run_id"`
	LoadPlanID    *uuid.UUID `json:"load_plan_id,omitempty"`
	Environment   string     `json:"environment"`
	FaultKind     string     `json:"fault_kind"`
	TargetKind    string     `json:"target_kind"`
	TargetRef     string     `json:"target_ref"`
	Status        string     `json:"status"`
	ApprovedBy    *int64     `json:"approved_by,omitempty"`
	AbortDeadline *time.Time `json:"abort_deadline,omitempty"`
}

type RadarFaultActionRequest struct {
	Action     string `json:"action"`
	FaultKind  string `json:"fault_kind"`
	TargetKind string `json:"target_kind"`
	TargetRef  string `json:"target_ref"`
}

type RadarFaultActionReceipt struct {
	ExperimentID uuid.UUID `json:"experiment_id"`
	Action       string    `json:"action"`
	OperationID  uuid.UUID `json:"operation_id"`
	Status       string    `json:"status"`
}

type RadarFaultEventSubmission struct {
	ExperimentID    uuid.UUID       `json:"experiment_id"`
	RunID           uuid.UUID       `json:"run_id"`
	EventType       string          `json:"event_type"`
	ActorID         *int64          `json:"actor_id,omitempty"`
	ServiceIdentity string          `json:"service_identity"`
	CauseEvent      string          `json:"cause_event"`
	CreatedAt       time.Time       `json:"created_at"`
	Payload         json.RawMessage `json:"payload"`
	EventHash       string          `json:"event_hash"`
}

type RadarFaultEventReceipt struct {
	Accepted  bool      `json:"accepted"`
	EventID   uuid.UUID `json:"event_id"`
	EventHash string    `json:"event_hash"`
	Status    string    `json:"status"`
}

type RadarRecoveryObservation struct {
	Observation json.RawMessage `json:"observation"`
}

type RadarRecoveryEvidenceSubmission struct {
	RunID               uuid.UUID       `json:"run_id"`
	ExperimentID        uuid.UUID       `json:"experiment_id"`
	RecoveryGeneration  int             `json:"recovery_generation"`
	SourceWatermark     string          `json:"source_watermark"`
	Status              string          `json:"status"`
	RPOms               *int64          `json:"rpo_ms,omitempty"`
	RTOms               *int64          `json:"rto_ms,omitempty"`
	DuplicateScoreCount int             `json:"duplicate_score_count"`
	DeterministicRunID  *uuid.UUID      `json:"deterministic_run_id,omitempty"`
	VerifiedBy          *int64          `json:"verified_by,omitempty"`
	VerifiedAt          time.Time       `json:"verified_at"`
	ReasonCodes         []string        `json:"reason_codes,omitempty"`
	CanonicalEvidence   json.RawMessage `json:"canonical_evidence_bytes"`
	EvidenceHash        string          `json:"evidence_hash"`
}

type RadarRecoveryEvidenceReceipt struct {
	EvidenceID   uuid.UUID `json:"evidence_id"`
	EvidenceHash string    `json:"evidence_hash"`
	Status       string    `json:"status"`
}

type RadarReliabilityExecutionRepository interface {
	GetApprovedFaultExperiment(context.Context, uuid.UUID) (*RadarFaultExperiment, error)
	ApplyFaultAction(context.Context, uuid.UUID, RadarFaultActionRequest) (*RadarFaultActionReceipt, error)
	AppendFaultEvent(context.Context, RadarFaultEventSubmission) (*RadarFaultEventReceipt, error)
	GetRecoveryObservation(context.Context, uuid.UUID) (*RadarRecoveryObservation, error)
	PublishRecoveryEvidence(context.Context, uuid.UUID, RadarRecoveryEvidenceSubmission) (*RadarRecoveryEvidenceReceipt, error)
}

type RadarLoadPlanInput struct {
	TenantID             int64           `json:"tenant_id"`
	Environment          string          `json:"environment"`
	RouteProfileVersion  string          `json:"route_profile_version"`
	ReliabilityProfileID string          `json:"reliability_profile_id,omitempty"`
	ModelAliases         []string        `json:"model_aliases"`
	Regions              []string        `json:"regions"`
	TrafficMode          string          `json:"traffic_mode"`
	ConcurrencyLevels    []int           `json:"concurrency_levels"`
	InputTokenBuckets    []int           `json:"input_token_buckets"`
	OutputTokenBuckets   []int           `json:"output_token_buckets"`
	WarmupSeconds        int             `json:"warmup_seconds"`
	MeasurementSeconds   int             `json:"measurement_seconds"`
	MinimumValidRequests int             `json:"minimum_valid_requests"`
	MaxRunCost           decimal.Decimal `json:"max_run_cost"`
	MaxConcurrency       int             `json:"max_concurrency"`
	ClientImageDigest    string          `json:"client_image_digest"`
	GeneratorVersion     string          `json:"generator_version"`
}

type CanonicalRadarLoadPlan struct {
	RadarLoadPlanInput
	SchemaVersion  string
	CanonicalBytes []byte
	SHA256         string
}

type RadarLoadPlanRecord struct {
	ID             uuid.UUID       `json:"id"`
	SchemaVersion  string          `json:"schema_version"`
	TenantID       int64           `json:"tenant_id"`
	CanonicalPlan  json.RawMessage `json:"canonical_plan"`
	LoadPlanSHA256 string          `json:"load_plan_sha256"`
	Status         string          `json:"status"`
	CreatedBy      int64           `json:"created_by"`
	PublishedAt    *time.Time      `json:"published_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type RadarReliabilityRepository interface {
	RadarAuthorizer
	CreateLoadPlan(ctx context.Context, input RadarLoadPlanInput, actorID int64) (*RadarLoadPlanRecord, error)
	PublishLoadPlan(ctx context.Context, id uuid.UUID, actorID int64) (*RadarLoadPlanRecord, error)
	GetLoadPlan(ctx context.Context, id uuid.UUID) (*RadarLoadPlanRecord, error)
}

type RadarTenantScopedReliabilityRepository interface {
	PublishLoadPlanForTenant(context.Context, uuid.UUID, int64, int64) (*RadarLoadPlanRecord, error)
	GetLoadPlanForTenant(context.Context, uuid.UUID, int64) (*RadarLoadPlanRecord, error)
}

// RadarReliabilityFactsRepository exposes the immutable references required by
// an external acceptance verifier. Implementations must load all fields from a
// repeatable read and apply the authenticated tenant predicate.
type RadarReliabilityFactsRepository interface {
	GetReliabilityFacts(context.Context, uuid.UUID, uuid.UUID, string) (*RadarReliabilityFacts, error)
}

type RadarReliabilityFacts struct {
	SchemaVersion          string                         `json:"schema_version"`
	RunID                  uuid.UUID                      `json:"run_id"`
	LoadPlanID             uuid.UUID                      `json:"load_plan_id"`
	LoadPlanSHA256         string                         `json:"load_plan_sha256"`
	ProfileID              string                         `json:"profile_id"`
	PolicyID               uuid.UUID                      `json:"policy_id"`
	PolicyHash             string                         `json:"policy_hash"`
	ReleaseSubjectID       uuid.UUID                      `json:"release_subject_id"`
	ReleaseSubjectHash     string                         `json:"release_subject_hash"`
	Snapshots              []RadarReliabilityFactSnapshot `json:"snapshots"`
	Recovery               *RadarRecoveryFact             `json:"recovery,omitempty"`
	ArtifactManifestHashes []string                       `json:"artifact_manifest_hashes"`
}

type RadarReliabilityFactSnapshot struct {
	ID              uuid.UUID       `json:"snapshot_id"`
	SnapshotHash    string          `json:"snapshot_hash"`
	RunID           uuid.UUID       `json:"run_id"`
	LoadPlanID      uuid.UUID       `json:"load_plan_id"`
	ProfileID       string          `json:"profile_id"`
	SliceKey        string          `json:"slice_key"`
	WindowStart     time.Time       `json:"window_start"`
	WindowEnd       time.Time       `json:"window_end"`
	QueryVersion    string          `json:"query_version"`
	SourceHash      string          `json:"source_hash"`
	SourceWatermark string          `json:"source_watermark"`
	FreshUntil      time.Time       `json:"fresh_until"`
	Metrics         json.RawMessage `json:"metrics"`
}

type RadarRecoveryFact struct {
	EvidenceID         uuid.UUID `json:"evidence_id"`
	EvidenceHash       string    `json:"evidence_hash"`
	RunID              uuid.UUID `json:"run_id"`
	ExperimentID       uuid.UUID `json:"experiment_id"`
	SourceWatermark    string    `json:"source_watermark"`
	RecoveryGeneration int       `json:"recovery_generation"`
}

type canonicalRadarLoadPlan struct {
	SchemaVersion        string   `json:"schema_version"`
	TenantID             int64    `json:"tenant_id"`
	Environment          string   `json:"environment"`
	RouteProfileVersion  string   `json:"route_profile_version"`
	ReliabilityProfileID string   `json:"reliability_profile_id,omitempty"`
	ModelAliases         []string `json:"model_aliases"`
	Regions              []string `json:"regions"`
	TrafficMode          string   `json:"traffic_mode"`
	ConcurrencyLevels    []int    `json:"concurrency_levels"`
	InputTokenBuckets    []int    `json:"input_token_buckets"`
	OutputTokenBuckets   []int    `json:"output_token_buckets"`
	WarmupSeconds        int      `json:"warmup_seconds"`
	MeasurementSeconds   int      `json:"measurement_seconds"`
	MinimumValidRequests int      `json:"minimum_valid_requests"`
	MaxRunCost           string   `json:"max_run_cost"`
	MaxConcurrency       int      `json:"max_concurrency"`
	ClientImageDigest    string   `json:"client_image_digest"`
	GeneratorVersion     string   `json:"generator_version"`
}

func CanonicalLoadPlan(input RadarLoadPlanInput) (CanonicalRadarLoadPlan, error) {
	if input.TenantID <= 0 {
		return CanonicalRadarLoadPlan{}, errors.New("load plan tenant is required")
	}

	input.Environment = strings.ToLower(strings.TrimSpace(input.Environment))
	input.RouteProfileVersion = strings.TrimSpace(input.RouteProfileVersion)
	input.ReliabilityProfileID = strings.TrimSpace(input.ReliabilityProfileID)
	input.TrafficMode = strings.ToLower(strings.TrimSpace(input.TrafficMode))
	input.ClientImageDigest = strings.TrimSpace(input.ClientImageDigest)
	input.GeneratorVersion = strings.TrimSpace(input.GeneratorVersion)
	if input.Environment == "" || len(input.Environment) > 32 {
		return CanonicalRadarLoadPlan{}, errors.New("load plan environment is required")
	}
	if input.RouteProfileVersion == "" || len(input.RouteProfileVersion) > 100 {
		return CanonicalRadarLoadPlan{}, errors.New("load plan route profile version is required")
	}
	if input.TrafficMode != "closed_loop" && input.TrafficMode != "open_loop" {
		return CanonicalRadarLoadPlan{}, errors.New("load plan traffic mode is invalid")
	}
	if input.GeneratorVersion == "" || len(input.GeneratorVersion) > 100 {
		return CanonicalRadarLoadPlan{}, errors.New("load plan generator version is required")
	}
	if !isLowerSHA256Digest(input.ClientImageDigest) {
		return CanonicalRadarLoadPlan{}, errors.New("load plan client image digest must be sha256")
	}

	var err error
	input.ModelAliases, err = canonicalLoadPlanStrings(input.ModelAliases, "model aliases")
	if err != nil {
		return CanonicalRadarLoadPlan{}, err
	}
	input.Regions, err = canonicalLoadPlanStrings(input.Regions, "regions")
	if err != nil {
		return CanonicalRadarLoadPlan{}, err
	}
	input.ConcurrencyLevels, err = canonicalLoadPlanInts(input.ConcurrencyLevels, "concurrency levels")
	if err != nil {
		return CanonicalRadarLoadPlan{}, err
	}
	input.InputTokenBuckets, err = canonicalLoadPlanInts(input.InputTokenBuckets, "input token buckets")
	if err != nil {
		return CanonicalRadarLoadPlan{}, err
	}
	input.OutputTokenBuckets, err = canonicalLoadPlanInts(input.OutputTokenBuckets, "output token buckets")
	if err != nil {
		return CanonicalRadarLoadPlan{}, err
	}
	if input.MaxConcurrency < 1 || input.MaxConcurrency > 100000 {
		return CanonicalRadarLoadPlan{}, errors.New("load plan maximum concurrency is out of range")
	}
	for _, level := range input.ConcurrencyLevels {
		if level > input.MaxConcurrency {
			return CanonicalRadarLoadPlan{}, errors.New("load plan concurrency exceeds maximum concurrency")
		}
	}
	if input.WarmupSeconds < 0 || input.WarmupSeconds > 86400 {
		return CanonicalRadarLoadPlan{}, errors.New("load plan warmup duration is out of range")
	}
	if input.MeasurementSeconds < 1 || input.MeasurementSeconds > 86400 {
		return CanonicalRadarLoadPlan{}, errors.New("load plan measurement duration is out of range")
	}
	if input.MinimumValidRequests < 1 {
		return CanonicalRadarLoadPlan{}, errors.New("load plan minimum request count is required")
	}
	if input.MaxRunCost.LessThanOrEqual(decimal.Zero) {
		return CanonicalRadarLoadPlan{}, errors.New("load plan maximum run cost must be positive")
	}

	document := canonicalRadarLoadPlan{
		SchemaVersion:        RadarLoadPlanSchemaV1,
		TenantID:             input.TenantID,
		Environment:          input.Environment,
		RouteProfileVersion:  input.RouteProfileVersion,
		ReliabilityProfileID: input.ReliabilityProfileID,
		ModelAliases:         input.ModelAliases,
		Regions:              input.Regions,
		TrafficMode:          input.TrafficMode,
		ConcurrencyLevels:    input.ConcurrencyLevels,
		InputTokenBuckets:    input.InputTokenBuckets,
		OutputTokenBuckets:   input.OutputTokenBuckets,
		WarmupSeconds:        input.WarmupSeconds,
		MeasurementSeconds:   input.MeasurementSeconds,
		MinimumValidRequests: input.MinimumValidRequests,
		MaxRunCost:           input.MaxRunCost.StringFixed(8),
		MaxConcurrency:       input.MaxConcurrency,
		ClientImageDigest:    input.ClientImageDigest,
		GeneratorVersion:     input.GeneratorVersion,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return CanonicalRadarLoadPlan{}, fmt.Errorf("marshal load plan: %w", err)
	}
	canonical, err := jsoncanonicalizer.Transform(encoded)
	if err != nil {
		return CanonicalRadarLoadPlan{}, fmt.Errorf("canonicalize load plan: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return CanonicalRadarLoadPlan{
		RadarLoadPlanInput: input,
		SchemaVersion:      RadarLoadPlanSchemaV1,
		CanonicalBytes:     append([]byte(nil), canonical...),
		SHA256:             hex.EncodeToString(digest[:]),
	}, nil
}

func canonicalLoadPlanStrings(values []string, label string) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("load plan %s are required", label)
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("load plan %s cannot contain empty values", label)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func canonicalLoadPlanInts(values []int, label string) ([]int, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("load plan %s are required", label)
	}
	seen := make(map[int]struct{}, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			return nil, fmt.Errorf("load plan %s must be positive", label)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Ints(result)
	return result, nil
}

func isLowerSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
