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
	// ErrWorkerKindMismatch is returned when a worker credential is used by a
	// queue owned by another worker kind. It deliberately does not reveal the
	// existence of a valid worker token to callers.
	ErrWorkerKindMismatch     = errors.New("evaluation worker kind is not authorized")
	ErrGradingLeaseFenced     = ErrLeaseFenced
	ErrAnalysisLeaseFenced    = ErrLeaseFenced
	ErrScoreSubmissionInvalid = errors.New("invalid score submission")
	ErrEvidenceMismatch       = errors.New("score evidence does not match stored evidence")
	ErrGraderIdentityMismatch = errors.New("score grader identity does not match evaluation case")
	ErrAggregateRunMismatch   = errors.New("aggregate score belongs to another evaluation run")
	ErrAnalysisJobFenced      = errors.New("evaluation analysis job lease fenced")
	ErrArtifactInvalid        = errors.New("invalid evaluation artifact")
	ErrArtifactNotFound       = errors.New("evaluation artifact not found")
	ErrArtifactObjectMismatch = errors.New("evaluation artifact object metadata mismatch")
)

// GradingLease is a short-lived, fenced lease owned by a grader worker.
type GradingLease struct {
	ID               uuid.UUID           `json:"id"`
	RunID            uuid.UUID           `json:"run_id"`
	SampleID         uuid.UUID           `json:"sample_id"`
	AssignmentID     uuid.UUID           `json:"assignment_id"`
	GraderID         string              `json:"grader_id"`
	GraderVersion    string              `json:"grader_version"`
	Attempt          int                 `json:"attempt"`
	EvidenceManifest json.RawMessage     `json:"evidence_manifest,omitempty"`
	Evidence         []ArtifactReceipt   `json:"evidence,omitempty"`
	RouteTraceID     string              `json:"route_trace_id,omitempty"`
	Case             *EvaluationCaseSpec `json:"case,omitempty"`
	Token            string              `json:"token"`
	ExpiresAt        time.Time           `json:"expires_at"`
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
}

// Score is an append-only grading result. A newer version clears IsCurrent on
// the preceding result while preserving it for audit and reproducibility.
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
	IsCurrent            bool            `json:"is_current"`
	ManualReviewRequired bool            `json:"manual_review_required"`
	CreatedAt            time.Time       `json:"created_at"`
}

// AnalysisJobLease is the statistics worker equivalent of GradingLease.
type AnalysisJobLease struct {
	ID               uuid.UUID               `json:"id"`
	RunID            uuid.UUID               `json:"run_id"`
	CapabilityDomain string                  `json:"capability_domain"`
	ModelRoute       string                  `json:"model_route"`
	Window           string                  `json:"window"`
	AnalysisVersion  string                  `json:"analysis_version"`
	WindowStart      time.Time               `json:"window_start"`
	Token            string                  `json:"token"`
	ExpiresAt        time.Time               `json:"expires_at"`
	ScoreIDs         []uuid.UUID             `json:"score_ids,omitempty"`
	Pairs            []PairedScore           `json:"pairs,omitempty"`
	History          []AggregateHistoryPoint `json:"history,omitempty"`
	InvalidFailures  []FailureClass          `json:"invalid_failures,omitempty"`
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
	RunID            uuid.UUID       `json:"run_id"`
	CapabilityDomain string          `json:"capability_domain,omitempty"`
	ModelRoute       string          `json:"model_route,omitempty"`
	Window           string          `json:"window,omitempty"`
	AnalysisVersion  string          `json:"analysis_version,omitempty"`
	WindowStart      time.Time       `json:"window_start,omitempty"`
	ScoreIDs         []uuid.UUID     `json:"score_ids"`
	Aggregate        json.RawMessage `json:"aggregate"`
}

type AggregateSnapshot struct {
	ID               uuid.UUID       `json:"id"`
	RunID            uuid.UUID       `json:"run_id"`
	CapabilityDomain string          `json:"capability_domain"`
	ModelRoute       string          `json:"model_route"`
	Window           string          `json:"window"`
	AnalysisVersion  string          `json:"analysis_version"`
	WindowStart      time.Time       `json:"window_start"`
	ScoreIDs         []uuid.UUID     `json:"score_ids"`
	Aggregate        json.RawMessage `json:"aggregate"`
	CreatedAt        time.Time       `json:"created_at"`
}

// EvaluationGradingRepository is the persistence contract shared by internal
// grader and statistics HTTP handlers. Worker kind is checked by
// AuthenticateWorker and checked again in each claim transaction.
type EvaluationGradingRepository interface {
	AuthenticateWorker(ctx context.Context, token, workerKind string) (uuid.UUID, error)
	ClaimGradingLease(ctx context.Context, workerID uuid.UUID, graderIDs []string, leaseTTL time.Duration) (*GradingLease, error)
	HeartbeatGradingLease(ctx context.Context, leaseID uuid.UUID, leaseToken string, extendBy time.Duration) (time.Time, error)
	SubmitScore(ctx context.Context, leaseID uuid.UUID, leaseToken string, submission ScoreSubmission) (*Score, error)
	FailGradingLease(ctx context.Context, leaseID uuid.UUID, leaseToken, failureClass, failureCode string) error
	ClaimAnalysisJob(ctx context.Context, workerID uuid.UUID, capabilities []string, leaseTTL time.Duration) (*AnalysisJobLease, error)
	CompleteAnalysisJob(ctx context.Context, jobID uuid.UUID, leaseToken string, submission AggregateSubmission) (*AggregateSnapshot, error)
}

type EvaluationRunnerRepository interface {
	AuthenticateRunner(ctx context.Context, token string) (uuid.UUID, error)
	ClaimAssignment(ctx context.Context, workerID uuid.UUID, capabilities []string, leaseTTL time.Duration) (*AssignmentLease, error)
	RenewAssignmentLease(ctx context.Context, assignmentID uuid.UUID, leaseToken string, extendBy time.Duration) (time.Time, error)
	SubmitEvidence(ctx context.Context, input EvidenceSubmission, leaseToken string) (*EvidenceReceipt, error)
	CompleteAssignment(ctx context.Context, assignmentID uuid.UUID, leaseToken string) error
	FailAssignment(ctx context.Context, assignmentID uuid.UUID, leaseToken, failureClass, failureCode string) error
}

type EvaluationArtifactRepository interface {
	PresignArtifact(ctx context.Context, assignmentID uuid.UUID, leaseToken string, input ArtifactPresignRequest) (*ArtifactUpload, error)
	ConfirmArtifact(ctx context.Context, assignmentID uuid.UUID, leaseToken string, input ArtifactConfirmation) (*ArtifactReceipt, error)
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
