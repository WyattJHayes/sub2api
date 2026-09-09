package service

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRevisionBatchInvalid             = errors.New("invalid evaluation revision batch request")
	ErrRevisionBatchRunNotCompleted     = errors.New("evaluation revision batch requires a completed run")
	ErrRevisionBatchConflict            = errors.New("evaluation run already has an active revision batch")
	ErrRevisionBatchFenced              = errors.New("evaluation revision batch writer is fenced")
	ErrRevisionBatchPropagationRequired = errors.New("evaluation revision batch propagation must complete")
	ErrRevisionBatchNotRepairable       = errors.New("evaluation revision batch is not repairable")
	ErrCompensatingHeadApprovalRequired = errors.New("compensating score head requires another approver")
)

var revisionBatchIdempotencyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type RevisionBatchStatus string

const (
	RevisionBatchPending   RevisionBatchStatus = "pending"
	RevisionBatchRunning   RevisionBatchStatus = "running"
	RevisionBatchBlocked   RevisionBatchStatus = "blocked"
	RevisionBatchCompleted RevisionBatchStatus = "completed"
	RevisionBatchFailed    RevisionBatchStatus = "failed"
	RevisionBatchCancelled RevisionBatchStatus = "cancelled"
)

func (status RevisionBatchStatus) Valid() bool {
	switch status {
	case RevisionBatchPending, RevisionBatchRunning, RevisionBatchBlocked,
		RevisionBatchCompleted, RevisionBatchFailed, RevisionBatchCancelled:
		return true
	default:
		return false
	}
}

func ValidateRevisionBatchIdempotencyKey(key string) error {
	if !revisionBatchIdempotencyPattern.MatchString(key) {
		return ErrRevisionBatchInvalid
	}
	return nil
}

type RevisionBatch struct {
	ID             uuid.UUID           `json:"id"`
	RunID          uuid.UUID           `json:"run_id"`
	Status         RevisionBatchStatus `json:"status"`
	ControlEpoch   int64               `json:"control_epoch"`
	Reason         string              `json:"reason"`
	RequestedBy    int64               `json:"requested_by"`
	IdempotencyKey string              `json:"idempotency_key"`
	StartedAt      *time.Time          `json:"started_at,omitempty"`
	FinishedAt     *time.Time          `json:"finished_at,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
}

type RevisionBatchRequirement struct {
	ID                    uuid.UUID  `json:"id"`
	RevisionBatchID       uuid.UUID  `json:"revision_batch_id"`
	RunID                 uuid.UUID  `json:"run_id"`
	RequirementType       string     `json:"requirement_type"`
	TargetKey             string     `json:"target_key"`
	SourceAssignmentID    uuid.UUID  `json:"source_assignment_id"`
	PreviousScore         ScoreRef   `json:"previous_score"`
	GraderID              string     `json:"grader_id"`
	GraderVersion         string     `json:"grader_version"`
	GradingInputHash      string     `json:"grading_input_hash"`
	SourceHash            string     `json:"source_hash"`
	CauseSetHash          string     `json:"cause_set_hash"`
	Status                string     `json:"status"`
	RecoveryGeneration    int        `json:"recovery_generation"`
	ReplacesRequirementID *uuid.UUID `json:"replaces_requirement_id,omitempty"`
}

type CreateRevisionBatchInput struct {
	RunID          uuid.UUID
	Reason         string
	RequestedBy    int64
	IdempotencyKey string
}

type RevisionBatchControlInput struct {
	BatchID        uuid.UUID
	Reason         string
	ActorID        int64
	IdempotencyKey string
}

type CompensatingScoreHeadInput struct {
	BatchID        uuid.UUID
	SampleID       uuid.UUID
	GraderID       string
	ScoreRef       ScoreRef
	ActorID        int64
	IdempotencyKey string
}

type CompensatingScoreHeadResult struct {
	BatchID       uuid.UUID `json:"revision_batch_id"`
	ApprovalCount int       `json:"approval_count"`
	Applied       bool      `json:"applied"`
	ScoreRef      ScoreRef  `json:"score_ref"`
	HeadVersion   int       `json:"head_version,omitempty"`
}

type RevisionBatchRepository interface {
	CreateRevisionBatch(ctx context.Context, input CreateRevisionBatchInput) (*RevisionBatch, error)
	FenceRevisionBatch(ctx context.Context, input RevisionBatchControlInput) (*RevisionBatch, error)
	ResumeRevisionBatch(ctx context.Context, input RevisionBatchControlInput) (*RevisionBatch, error)
	CancelRevisionBatch(ctx context.Context, input RevisionBatchControlInput) (*RevisionBatch, error)
	RepairRevisionBatch(ctx context.Context, input RevisionBatchControlInput) (*RevisionBatch, error)
	ApproveCompensatingScoreHead(ctx context.Context, input CompensatingScoreHeadInput) (*CompensatingScoreHeadResult, error)
}
