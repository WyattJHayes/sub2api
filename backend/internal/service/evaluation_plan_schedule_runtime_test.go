//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type evaluationScheduleStoreStub struct {
	mu        sync.Mutex
	claims    []ScheduledEvaluationPlanClaim
	claimErr  error
	claimCall int
	complete  []evaluationScheduleCompletion
	recordErr error
}

type evaluationScheduleCompletion struct {
	PlanID    uuid.UUID
	Token     string
	RanAt     time.Time
	NextRunAt time.Time
}

func (s *evaluationScheduleStoreStub) ClaimDueScheduledPlans(_ context.Context, _ time.Time, _ int, _ time.Duration) ([]ScheduledEvaluationPlanClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCall++
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	claims := s.claims
	s.claims = nil
	return claims, nil
}

func (s *evaluationScheduleStoreStub) CompleteScheduledPlanClaim(_ context.Context, planID uuid.UUID, leaseToken string, ranAt, nextRunAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return s.recordErr
	}
	s.complete = append(s.complete, evaluationScheduleCompletion{
		PlanID: planID, Token: leaseToken, RanAt: ranAt, NextRunAt: nextRunAt,
	})
	return nil
}

func (s *evaluationScheduleStoreStub) completions() []evaluationScheduleCompletion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]evaluationScheduleCompletion(nil), s.complete...)
}

type evaluationRunCreatorStub struct {
	mu      sync.Mutex
	inputs  []CreateRunInput
	context []context.Context
	err     error
}

func (s *evaluationRunCreatorStub) CreateRunWithMatrix(ctx context.Context, input CreateRunInput) (*EvaluationRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs = append(s.inputs, input)
	s.context = append(s.context, ctx)
	if s.err != nil {
		return nil, s.err
	}
	return &EvaluationRun{ID: uuid.New(), PlanID: input.PlanID, Status: RunStatusPending}, nil
}

func (s *evaluationRunCreatorStub) createdInputs() []CreateRunInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CreateRunInput(nil), s.inputs...)
}

func newDueScheduleClaim(t *testing.T, cronExpr string, baseline, candidate string) ScheduledEvaluationPlanClaim {
	t.Helper()
	return ScheduledEvaluationPlanClaim{
		PlanID:         uuid.New(),
		CronExpression: cronExpr,
		BaselineRef:    json.RawMessage(baseline),
		CandidateRef:   json.RawMessage(candidate),
		CreatedBy:      41,
		TenantID:       41,
		ScheduledFor:   time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC),
		LeaseToken:     uuid.NewString(),
	}
}

func TestEvaluationPlanScheduleRuntimeCreatesRunWithCronTrigger(t *testing.T) {
	claim := newDueScheduleClaim(t, "0 9 * * *", `{"release":"v0.2.7-sol"}`, `{"release":"v0.2.7-4models"}`)
	store := &evaluationScheduleStoreStub{claims: []ScheduledEvaluationPlanClaim{claim}}
	creator := &evaluationRunCreatorStub{}

	runtime := NewEvaluationPlanScheduleRuntime(store, creator, EvaluationPlanScheduleRuntimeOptions{Enabled: true})
	result, err := runtime.ProcessDue(context.Background())

	require.NoError(t, err)
	require.Equal(t, EvaluationPlanScheduleRunResult{Selected: 1, Started: 1}, result)

	inputs := creator.createdInputs()
	require.Len(t, inputs, 1)
	require.Equal(t, claim.PlanID, inputs[0].PlanID)
	require.Equal(t, "cron", inputs[0].TriggerSource)
	require.Equal(t, int64(41), inputs[0].CreatedBy)
	require.Equal(t, "v0.2.7-sol", inputs[0].BaselineRef["release"])
	require.Equal(t, "v0.2.7-4models", inputs[0].CandidateRef["release"])

	completions := store.completions()
	require.Len(t, completions, 1)
	require.Equal(t, claim.LeaseToken, completions[0].Token)
	require.True(t, completions[0].NextRunAt.After(completions[0].RanAt), "next run must move forward")
}

func TestEvaluationPlanScheduleRuntimeSkipsPlanWithoutConfiguredReferences(t *testing.T) {
	claim := newDueScheduleClaim(t, "0 9 * * *", `{}`, `{"release":"v0.2.7-4models"}`)
	store := &evaluationScheduleStoreStub{claims: []ScheduledEvaluationPlanClaim{claim}}
	creator := &evaluationRunCreatorStub{}

	runtime := NewEvaluationPlanScheduleRuntime(store, creator, EvaluationPlanScheduleRuntimeOptions{Enabled: true})
	result, err := runtime.ProcessDue(context.Background())

	require.NoError(t, err)
	require.Equal(t, EvaluationPlanScheduleRunResult{Selected: 1, Skipped: 1}, result)
	require.Empty(t, creator.createdInputs())
	require.Len(t, store.completions(), 1, "skipped plan must release its lease and advance")
}

func TestEvaluationPlanScheduleRuntimeSkipsPlanWithUnknownCronExpression(t *testing.T) {
	claim := newDueScheduleClaim(t, "not-a-cron", `{"release":"a"}`, `{"release":"b"}`)
	store := &evaluationScheduleStoreStub{claims: []ScheduledEvaluationPlanClaim{claim}}
	creator := &evaluationRunCreatorStub{}

	runtime := NewEvaluationPlanScheduleRuntime(store, creator, EvaluationPlanScheduleRuntimeOptions{Enabled: true})
	result, err := runtime.ProcessDue(context.Background())

	require.NoError(t, err)
	require.Equal(t, EvaluationPlanScheduleRunResult{Selected: 1, Skipped: 1}, result)
	require.Empty(t, creator.createdInputs())
	require.Len(t, store.completions(), 1)
	require.True(t, store.completions()[0].NextRunAt.After(store.completions()[0].RanAt))
}

func TestEvaluationPlanScheduleRuntimeAdvancesScheduleWhenRunCreationFails(t *testing.T) {
	claim := newDueScheduleClaim(t, "0 9 * * *", `{"release":"a"}`, `{"release":"b"}`)
	store := &evaluationScheduleStoreStub{claims: []ScheduledEvaluationPlanClaim{claim}}
	creator := &evaluationRunCreatorStub{err: errors.New("budget exceeded")}

	runtime := NewEvaluationPlanScheduleRuntime(store, creator, EvaluationPlanScheduleRuntimeOptions{Enabled: true})
	result, err := runtime.ProcessDue(context.Background())

	require.Error(t, err)
	require.Equal(t, EvaluationPlanScheduleRunResult{Selected: 1, Failed: 1}, result)
	completions := store.completions()
	require.Len(t, completions, 1, "failed plan must still be released to avoid a tight retry loop")
	require.True(t, completions[0].NextRunAt.After(completions[0].RanAt))
}

func TestEvaluationPlanScheduleRuntimeDisabledDoesNotClaim(t *testing.T) {
	store := &evaluationScheduleStoreStub{}
	creator := &evaluationRunCreatorStub{}

	runtime := NewEvaluationPlanScheduleRuntime(store, creator, EvaluationPlanScheduleRuntimeOptions{Enabled: false})
	result, err := runtime.ProcessDue(context.Background())

	require.NoError(t, err)
	require.Equal(t, EvaluationPlanScheduleRunResult{}, result)
	require.Zero(t, store.claimCall, "disabled runtime must not claim plans")
}

func TestEvaluationPlanScheduleRuntimePreventsOverlappingRuns(t *testing.T) {
	claim := newDueScheduleClaim(t, "0 9 * * *", `{"release":"a"}`, `{"release":"b"}`)
	blocking := &blockingEvaluationRunCreator{release: make(chan struct{}), entered: make(chan struct{})}
	store := &evaluationScheduleStoreStub{claims: []ScheduledEvaluationPlanClaim{claim}}

	runtime := NewEvaluationPlanScheduleRuntime(store, blocking, EvaluationPlanScheduleRuntimeOptions{Enabled: true})
	go func() { _, _ = runtime.ProcessDue(context.Background()) }()
	<-blocking.entered

	result, err := runtime.ProcessDue(context.Background())
	require.NoError(t, err)
	require.Equal(t, EvaluationPlanScheduleRunResult{}, result, "overlapping tick must be a no-op")
	close(blocking.release)
}

type blockingEvaluationRunCreator struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockingEvaluationRunCreator) CreateRunWithMatrix(_ context.Context, _ CreateRunInput) (*EvaluationRun, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return &EvaluationRun{ID: uuid.New(), Status: RunStatusPending}, nil
}

func TestNextEvaluationPlanRunUsesFiveFieldCron(t *testing.T) {
	from := time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC)
	next, err := NextEvaluationPlanRun("0 9 * * *", from)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), next)

	next, err = NextEvaluationPlanRun("*/15 * * * *", from)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 9, 20, 8, 45, 0, 0, time.UTC), next)

	_, err = NextEvaluationPlanRun("0 9 * * * *", from)
	require.Error(t, err, "six-field cron must be rejected to keep the contract five-field")
}
