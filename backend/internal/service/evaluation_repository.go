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
	ErrLeaseFenced    = errors.New("evaluation assignment lease fenced")
	ErrBudgetExceeded = errors.New("evaluation run budget exceeded")
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
	ID           uuid.UUID
	PlanID       uuid.UUID
	Status       RunStatus
	BudgetLimit  decimal.Decimal
	ReservedCost decimal.Decimal
	CreatedAt    time.Time
}

type AssignmentLease struct {
	ID                uuid.UUID
	SampleID          uuid.UUID
	RunID             uuid.UUID
	ModelRoute        string
	ModelConfig       json.RawMessage
	ModelConfigSHA256 string
	Attempt           int
	Token             string
	ExpiresAt         time.Time
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
}
