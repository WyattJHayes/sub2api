package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	// ErrWorkerKindMismatch is returned when a worker credential is used by a
	// queue owned by another worker kind. It deliberately does not reveal the
	// existence of a valid worker token to callers.
	ErrWorkerKindMismatch                = errors.New("evaluation worker kind is not authorized")
	ErrGradingLeaseFenced                = ErrLeaseFenced
	ErrAnalysisLeaseFenced               = ErrLeaseFenced
	ErrScoreSubmissionInvalid            = errors.New("invalid score submission")
	ErrEvidenceMismatch                  = errors.New("score evidence does not match stored evidence")
	ErrRouteEvidenceNotSealed            = errors.New("route evidence is not sealed")
	ErrGraderIdentityMismatch            = errors.New("score grader identity does not match evaluation case")
	ErrAggregateRunMismatch              = errors.New("aggregate score belongs to another evaluation run")
	ErrAnalysisJobFenced                 = errors.New("evaluation analysis job lease fenced")
	ErrAnalysisJobInvalid                = errors.New("invalid evaluation analysis job failure")
	ErrArtifactInvalid                   = errors.New("invalid evaluation artifact")
	ErrArtifactNotFound                  = errors.New("evaluation artifact not found")
	ErrArtifactObjectMismatch            = errors.New("evaluation artifact object metadata mismatch")
	ErrArtifactObjectStoreUnavailable    = errors.New("evaluation artifact object store unavailable")
	ErrArtifactObjectMetadataUnavailable = errors.New("evaluation artifact object metadata unavailable")
	ErrArtifactScannerUnavailable        = errors.New("evaluation artifact scanner unavailable")
	ErrArtifactScanRejected              = errors.New("evaluation artifact scan rejected")
	ErrArtifactScanFailed                = errors.New("evaluation artifact scan failed")
	ErrWorkerIdentityMismatch            = errors.New("evaluation worker identity does not match")
)

// GradingLease is a short-lived, fenced lease owned by a grader worker.
type GradingLease struct {
	ID                 uuid.UUID           `json:"id"`
	RunID              uuid.UUID           `json:"run_id"`
	SampleID           uuid.UUID           `json:"sample_id"`
	AssignmentID       uuid.UUID           `json:"assignment_id"`
	GraderID           string              `json:"grader_id"`
	GraderVersion      string              `json:"grader_version"`
	Attempt            int                 `json:"attempt"`
	EvidenceManifest   json.RawMessage     `json:"evidence_manifest,omitempty"`
	Evidence           []ArtifactReceipt   `json:"evidence,omitempty"`
	RouteTraceID       string              `json:"route_trace_id,omitempty"`
	Case               *EvaluationCaseSpec `json:"case,omitempty"`
	Token              string              `json:"token"`
	ExpiresAt          time.Time           `json:"expires_at"`
	LeaseEpoch         int64               `json:"lease_epoch"`
	WorkerImageDigest  string              `json:"worker_image_digest,omitempty"`
	WorkOrigin         string              `json:"work_origin,omitempty"`
	RevisionBatchID    uuid.UUID           `json:"revision_batch_id,omitempty"`
	GradingInputHash   string              `json:"grading_input_hash,omitempty"`
	RecoveryGeneration int                 `json:"recovery_generation,omitempty"`
}

// ScoreSubmission is immutable input accepted by one grading lease. Decimal
// is retained through the persistence boundary so 0..1 comparisons do not
// depend on floating point rounding.
type ScoreSubmission struct {
	SampleID       uuid.UUID       `json:"sample_id"`
	GraderID       string          `json:"grader_id"`
	GraderVersion  string          `json:"grader_version"`
	Score          decimal.Decimal `json:"score"`
	Passed         *bool           `json:"passed,omitempty"`
	FailureClass   FailureClass    `json:"failure_class,omitempty"`
	FailureCode    string          `json:"failure_code,omitempty"`
	Explanation    string          `json:"explanation,omitempty"`
	EvidenceHashes []string        `json:"evidence_hashes"`
	LeaseEpoch     int64           `json:"lease_epoch"`
}

// ScoreRef is the complete locator for a partitioned immutable score row.
type ScoreRef struct {
	ID        uuid.UUID `json:"score_id"`
	CreatedAt time.Time `json:"score_created_at"`
}

// RouteEvidenceRef identifies one sealed gateway result without exposing its payload.
type RouteEvidenceRef struct {
	RouteTraceID   string `json:"route_trace_id"`
	RequestOrdinal int    `json:"request_ordinal"`
	PayloadHash    string `json:"payload_hash"`
}

// ScoreSource binds a score to the current assignment and its sealed inputs.
type ScoreSource struct {
	AssignmentID         uuid.UUID          `json:"assignment_id"`
	RouteEvidenceSetHash string             `json:"route_evidence_set_hash"`
	RouteEvidenceRefs    []RouteEvidenceRef `json:"route_evidence_refs"`
	ArtifactManifestHash string             `json:"artifact_manifest_hash"`
}

// Score is an append-only grading result addressed by its immutable ScoreRef.
type Score struct {
	ID                   uuid.UUID       `json:"id"`
	RunID                uuid.UUID       `json:"run_id"`
	SampleID             uuid.UUID       `json:"sample_id"`
	GraderID             string          `json:"grader_id"`
	GraderVersion        string          `json:"grader_version"`
	Version              int             `json:"version"`
	Score                decimal.Decimal `json:"score"`
	Passed               *bool           `json:"passed,omitempty"`
	FailureClass         FailureClass    `json:"failure_class,omitempty"`
	FailureCode          string          `json:"failure_code,omitempty"`
	Explanation          string          `json:"explanation,omitempty"`
	EvidenceHashes       []string        `json:"evidence_hashes"`
	ManualReviewRequired bool            `json:"manual_review_required"`
	CreatedAt            time.Time       `json:"created_at"`
	Ref                  ScoreRef        `json:"score_ref"`
	HeadVersion          int             `json:"head_version"`
	Source               ScoreSource     `json:"source"`
}

// AnalysisJobLease is the statistics worker equivalent of GradingLease.
type AnalysisJobLease struct {
	ID                uuid.UUID               `json:"id"`
	RunID             uuid.UUID               `json:"run_id"`
	CapabilityDomain  string                  `json:"capability_domain"`
	ModelRoute        string                  `json:"model_route"`
	Window            string                  `json:"window"`
	AnalysisVersion   string                  `json:"analysis_version"`
	WindowStart       time.Time               `json:"window_start"`
	Token             string                  `json:"token"`
	ExpiresAt         time.Time               `json:"expires_at"`
	LeaseEpoch        int64                   `json:"lease_epoch"`
	WorkerImageDigest string                  `json:"worker_image_digest,omitempty"`
	WorkOrigin        string                  `json:"work_origin,omitempty"`
	ScoreIDs          []uuid.UUID             `json:"score_ids,omitempty"`
	Pairs             []PairedScore           `json:"pairs,omitempty"`
	History           []AggregateHistoryPoint `json:"history,omitempty"`
	InvalidFailures   []FailureClass          `json:"invalid_failures,omitempty"`
	Scope             string                  `json:"scope"`
	InputSetHash      string                  `json:"input_set_hash"`
	ScoreRefs         []ScoreRef              `json:"score_refs,omitempty"`
	SnapshotRefs      []SnapshotRef           `json:"snapshot_refs,omitempty"`
	AggregateRevision int64                   `json:"aggregate_revision"`
	RevisionBatchID   uuid.UUID               `json:"revision_batch_id,omitempty"`
	QualityContext    *QualityAnalysisContext `json:"quality_context,omitempty"`
}

// QualityAnalysisContext contains only digest-safe, frozen inputs for quality
// analysis. It deliberately has no relationship to PairedScore.
type QualityAnalysisContext struct {
	RunID            uuid.UUID                       `json:"run_id"`
	ModelAlias       string                          `json:"model_alias"`
	PolicyVersion    string                          `json:"policy_version"`
	Policy           QualityPolicy                   `json:"policy"`
	Dimensions       []QualityAnalysisDimensionInput `json:"dimensions"`
	SourceCandidates []QualitySourceCandidateInput   `json:"source_candidates"`
}

type QualityAnalysisDimensionInput struct {
	Key                      QualityDimension       `json:"key"`
	BaselineScore            decimal.Decimal        `json:"baseline_score"`
	CandidateScore           decimal.Decimal        `json:"candidate_score"`
	SampleCount              int                    `json:"sample_count"`
	ReferenceBaselineDeltaPP *decimal.Decimal       `json:"reference_baseline_delta_pp,omitempty"`
	StableBaselineDeltaPP    *decimal.Decimal       `json:"stable_baseline_delta_pp,omitempty"`
	ProbeEventClass          QualityProbeEventClass `json:"probe_event_class"`
	ProbeSpecHash            string                 `json:"probe_spec_hash"`
	ObservationHash          string                 `json:"observation_hash"`
	ObservedAt               time.Time              `json:"observed_at"`
}

// QualitySourceCandidateInput is the digest-safe fingerprint evidence for one
// controlled source candidate in a frozen quality context.
type QualitySourceCandidateInput struct {
	DisplayName     string                 `json:"display_name"`
	Confidence      float64                `json:"confidence"`
	SampleCount     int                    `json:"sample_count"`
	BaselineScore   decimal.Decimal        `json:"baseline_score"`
	CandidateScore  decimal.Decimal        `json:"candidate_score"`
	ProbeEventClass QualityProbeEventClass `json:"probe_event_class"`
	ProbeSpecHash   string                 `json:"probe_spec_hash"`
	ObservationHash string                 `json:"observation_hash"`
	ObservedAt      time.Time              `json:"observed_at"`
}

type PairedScore struct {
	CaseID         uuid.UUID       `json:"case_id"`
	ModelRoute     string          `json:"model_route"`
	SampleIndex    int             `json:"sample_index"`
	Weight         decimal.Decimal `json:"weight"`
	BaselineScore  decimal.Decimal `json:"baseline_score"`
	CandidateScore decimal.Decimal `json:"candidate_score"`
}

type AggregateHistoryPoint struct {
	DeltaPP decimal.Decimal `json:"delta_pp"`
}

// AggregateSubmission identifies the score set used to produce one immutable
// aggregate snapshot. Score IDs are checked against the leased job's run in a
// single transaction before the snapshot is accepted.
type AggregateSubmission struct {
	RunID            uuid.UUID                 `json:"run_id"`
	CapabilityDomain string                    `json:"capability_domain,omitempty"`
	ModelRoute       string                    `json:"model_route,omitempty"`
	Window           string                    `json:"window,omitempty"`
	AnalysisVersion  string                    `json:"analysis_version,omitempty"`
	WindowStart      time.Time                 `json:"window_start,omitempty"`
	ScoreIDs         []uuid.UUID               `json:"score_ids"`
	ScoreRefs        []ScoreRef                `json:"score_refs,omitempty"`
	SnapshotRefs     []SnapshotRef             `json:"snapshot_refs,omitempty"`
	InputSetHash     string                    `json:"input_set_hash,omitempty"`
	Aggregate        json.RawMessage           `json:"aggregate"`
	LeaseEpoch       int64                     `json:"lease_epoch"`
	QualityReport    *QualityReportPublication `json:"quality_report,omitempty"`
}

type AggregateSnapshot struct {
	ID                uuid.UUID       `json:"id"`
	RunID             uuid.UUID       `json:"run_id"`
	CapabilityDomain  string          `json:"capability_domain"`
	ModelRoute        string          `json:"model_route"`
	Window            string          `json:"window"`
	AnalysisVersion   string          `json:"analysis_version"`
	WindowStart       time.Time       `json:"window_start"`
	ScoreIDs          []uuid.UUID     `json:"score_ids"`
	Aggregate         json.RawMessage `json:"aggregate"`
	CreatedAt         time.Time       `json:"created_at"`
	Ref               SnapshotRef     `json:"snapshot_ref"`
	InputSetHash      string          `json:"input_set_hash"`
	AggregateRevision int64           `json:"aggregate_revision"`
	AggregateHash     string          `json:"aggregate_hash"`
	ScoreRefs         []ScoreRef      `json:"score_refs,omitempty"`
	SourceSnapshots   []SnapshotRef   `json:"source_snapshot_refs,omitempty"`
	HeadAdvanced      bool            `json:"head_advanced"`
}

// EvaluationGradingRepository is the persistence contract shared by internal
// grader and statistics HTTP handlers. Worker kind is checked by
// AuthenticateWorker and checked again in each claim transaction.
type EvaluationGradingRepository interface {
	AuthenticateWorker(ctx context.Context, token, workerKind string) (uuid.UUID, error)
	ClaimGradingLease(ctx context.Context, workerID uuid.UUID, graderIDs []string, leaseTTL time.Duration) (*GradingLease, error)
	HeartbeatGradingLease(ctx context.Context, leaseID uuid.UUID, leaseToken string, extendBy time.Duration, leaseEpoch ...int64) (time.Time, error)
	SubmitScore(ctx context.Context, leaseID uuid.UUID, leaseToken string, submission ScoreSubmission) (*Score, error)
	FailGradingLease(ctx context.Context, leaseID uuid.UUID, leaseToken, failureClass, failureCode string, leaseEpoch ...int64) error
	ClaimAnalysisJob(ctx context.Context, workerID uuid.UUID, capabilities []string, leaseTTL time.Duration) (*AnalysisJobLease, error)
	CompleteAnalysisJob(ctx context.Context, jobID uuid.UUID, leaseToken string, submission AggregateSubmission, leaseEpoch ...int64) (*AggregateSnapshot, error)
}

type AnalysisFailureRepository interface {
	FailAnalysisJob(ctx context.Context, jobID uuid.UUID, leaseToken, failureCode string, leaseEpoch ...int64) error
}

type EvaluationRunnerRepository interface {
	AuthenticateRunner(ctx context.Context, token string) (uuid.UUID, error)
	ClaimAssignment(ctx context.Context, workerID uuid.UUID, capabilities []string, leaseTTL time.Duration) (*AssignmentLease, error)
	RenewAssignmentLease(ctx context.Context, assignmentID uuid.UUID, leaseToken string, extendBy time.Duration, leaseEpoch ...int64) (time.Time, error)
	SubmitEvidence(ctx context.Context, input EvidenceSubmission, leaseToken string) (*EvidenceReceipt, error)
	CompleteAssignment(ctx context.Context, assignmentID uuid.UUID, leaseToken string, leaseEpoch ...int64) error
	FailAssignment(ctx context.Context, assignmentID uuid.UUID, leaseToken, failureClass, failureCode string, leaseEpoch ...int64) error
}

// EvaluationWorkerHeartbeatRepository refreshes the liveness marker for an
// authenticated worker even when its queue is empty. It is kept separate from
// the runner and grading contracts so non-HTTP callers can adopt it
// independently.
type EvaluationWorkerHeartbeatRepository interface {
	TouchWorkerHeartbeat(ctx context.Context, workerID uuid.UUID, workerKind string) error
}

type EvaluationArtifactRepository interface {
	PresignArtifact(ctx context.Context, assignmentID uuid.UUID, leaseToken string, input ArtifactPresignRequest) (*ArtifactUpload, error)
	ConfirmArtifact(ctx context.Context, assignmentID uuid.UUID, leaseToken string, input ArtifactConfirmation) (*ArtifactReceipt, error)
}

type ArtifactDownload struct {
	ArtifactID  uuid.UUID `json:"artifact_id"`
	ObjectKey   string    `json:"object_key"`
	DownloadURL string    `json:"download_url"`
	SHA256      string    `json:"sha256"`
	Bytes       int64     `json:"bytes"`
	MIMEType    string    `json:"mime_type"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type EvaluationArtifactReadRepository interface {
	PresignGradingArtifactRead(ctx context.Context, workerID, leaseID uuid.UUID, leaseToken string, artifactID uuid.UUID, leaseEpoch int64) (*ArtifactDownload, error)
}

// ArtifactObjectPutRequest is the immutable metadata that must be signed into
// an artifact upload request. The object store is checked against the same
// values before an artifact can enter the clean state.
type ArtifactObjectPutRequest struct {
	ObjectKey string
	Bytes     int64
	MIMEType  string
	SHA256    string
}

type ArtifactObjectUpload struct {
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

type ArtifactObjectMetadata struct {
	ObjectKey string
	Bytes     int64
	MIMEType  string
	SHA256    string
	ETag      string
}

// EvaluationArtifactObjectStore is the byte store boundary for Radar
// evidence. Implementations must fail when object metadata cannot prove the
// requested checksum and length.
type EvaluationArtifactObjectStore interface {
	PresignPut(ctx context.Context, request ArtifactObjectPutRequest, expiry time.Duration) (*ArtifactObjectUpload, error)
	Head(ctx context.Context, objectKey string) (*ArtifactObjectMetadata, error)
	PresignGet(ctx context.Context, objectKey string, expiry time.Duration) (url string, expiresAt time.Time, err error)
	Delete(ctx context.Context, objectKey string) error
	Open(ctx context.Context, objectKey string) (io.ReadCloser, error)
}

type ArtifactScanStatus string

const (
	ArtifactScanClean    ArtifactScanStatus = "clean"
	ArtifactScanRejected ArtifactScanStatus = "rejected"
	ArtifactScanFailed   ArtifactScanStatus = "failed"
)

type ArtifactScanResult struct {
	Status    ArtifactScanStatus
	Scanner   string
	Reason    string
	ScannedAt time.Time
}

type ArtifactScanner interface {
	Scan(ctx context.Context, objectKey string, metadata ArtifactObjectMetadata) (ArtifactScanResult, error)
}

// GradingRepository is retained as a short alias for callers that use the
// repository naming convention used by earlier radar components.
type GradingRepository = EvaluationGradingRepository

// EvaluationGradingService keeps input validation in one place for future
// worker transports while leaving fencing and atomicity to the repository.
type EvaluationGradingService struct {
	repo EvaluationGradingRepository
}

func NewEvaluationGradingService(repo EvaluationGradingRepository) *EvaluationGradingService {
	return &EvaluationGradingService{repo: repo}
}

func (s *EvaluationGradingService) Repository() EvaluationGradingRepository {
	if s == nil {
		return nil
	}
	return s.repo
}
