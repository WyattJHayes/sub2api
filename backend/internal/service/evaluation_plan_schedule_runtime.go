package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

// evaluationPlanCronParser keeps the plan schedule contract to the same
// five-field cron syntax the existing scheduled-test runner accepts.
var evaluationPlanCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ErrEvaluationPlanScheduleFenced reports that a schedule lease was already
// replaced or released, so a stale runner must not advance the plan.
var ErrEvaluationPlanScheduleFenced = errors.New("evaluation plan schedule lease is fenced")

// ScheduledEvaluationPlanClaim is one due cron plan claimed with a lease. The
// lease token fences completion so a stale runner cannot advance a schedule a
// newer runner already owns.
type ScheduledEvaluationPlanClaim struct {
	PlanID         uuid.UUID
	CronExpression string
	BaselineRef    json.RawMessage
	CandidateRef   json.RawMessage
	CreatedBy      int64
	TenantID       int64
	ScheduledFor   time.Time
	LeaseToken     string
}

// EvaluationPlanScheduleStore owns due-plan discovery and lease completion.
// Implementations must claim each due plan at most once per scheduled instant.
type EvaluationPlanScheduleStore interface {
	ClaimDueScheduledPlans(ctx context.Context, now time.Time, limit int, leaseTTL time.Duration) ([]ScheduledEvaluationPlanClaim, error)
	CompleteScheduledPlanClaim(ctx context.Context, planID uuid.UUID, leaseToken string, ranAt, nextRunAt time.Time) error
}

// EvaluationRunCreator is the narrow run-creation port the schedule runtime
// needs. It is satisfied by the existing evaluation repository, so cron runs
// reuse every budget, dataset, and evaluation-key guard already enforced there.
type EvaluationRunCreator interface {
	CreateRunWithMatrix(ctx context.Context, input CreateRunInput) (*EvaluationRun, error)
}

type EvaluationPlanScheduleRuntimeOptions struct {
	Enabled        bool
	ClaimBatch     int
	LeaseDuration  time.Duration
	TickInterval   time.Duration
	ShutdownBudget time.Duration
}

type EvaluationPlanScheduleRunResult struct {
	Selected int
	Started  int
	Skipped  int
	Failed   int
}

const (
	defaultEvaluationPlanScheduleClaimBatch = 16
	defaultEvaluationPlanScheduleLease      = 5 * time.Minute
)

// EvaluationPlanScheduleRuntime reconciles due cron plans. It is intentionally
// non-overlapping: a tick that arrives while the previous tick still runs is
// dropped, so a slow evaluation never stacks duplicate runs.
type EvaluationPlanScheduleRuntime struct {
	store   EvaluationPlanScheduleStore
	creator EvaluationRunCreator
	options EvaluationPlanScheduleRuntimeOptions

	running atomic.Bool

	lifecycleMu sync.Mutex
	started     bool
	cancel      context.CancelFunc
	runtimeWG   sync.WaitGroup

	now func() time.Time
}

func NewEvaluationPlanScheduleRuntime(
	store EvaluationPlanScheduleStore,
	creator EvaluationRunCreator,
	options EvaluationPlanScheduleRuntimeOptions,
) *EvaluationPlanScheduleRuntime {
	return &EvaluationPlanScheduleRuntime{
		store:   store,
		creator: creator,
		options: normalizeEvaluationPlanScheduleOptions(options),
		now:     time.Now,
	}
}

func normalizeEvaluationPlanScheduleOptions(options EvaluationPlanScheduleRuntimeOptions) EvaluationPlanScheduleRuntimeOptions {
	if options.ClaimBatch <= 0 {
		options.ClaimBatch = defaultEvaluationPlanScheduleClaimBatch
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = defaultEvaluationPlanScheduleLease
	}
	if options.TickInterval <= 0 {
		options.TickInterval = time.Minute
	}
	if options.ShutdownBudget <= 0 {
		options.ShutdownBudget = 5 * time.Second
	}
	return options
}

// NextEvaluationPlanRun computes the next firing instant for a five-field cron
// expression. It rejects six-field (with-seconds) expressions so stored plans
// cannot silently gain sub-minute cadence.
func NextEvaluationPlanRun(expression string, from time.Time) (time.Time, error) {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return time.Time{}, errors.New("evaluation plan cron expression is required")
	}
	schedule, err := evaluationPlanCronParser.Parse(trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse evaluation plan cron expression: %w", err)
	}
	next := schedule.Next(from)
	if next.IsZero() {
		return time.Time{}, errors.New("evaluation plan cron expression has no next run")
	}
	return next, nil
}

func (r *EvaluationPlanScheduleRuntime) Start() {
	if r == nil || !r.options.Enabled || r.store == nil || r.creator == nil {
		return
	}
	r.lifecycleMu.Lock()
	if r.started {
		r.lifecycleMu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	r.started = true
	r.cancel = cancel
	interval := r.options.TickInterval
	r.lifecycleMu.Unlock()

	r.runtimeWG.Add(1)
	go func() {
		defer r.runtimeWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				if _, err := r.ProcessDue(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
					logEvaluationPlanScheduleError(err)
				}
			}
		}
	}()
}

func (r *EvaluationPlanScheduleRuntime) Stop() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	if !r.started {
		r.lifecycleMu.Unlock()
		return
	}
	r.started = false
	cancel := r.cancel
	r.cancel = nil
	r.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.runtimeWG.Wait()
}

func (r *EvaluationPlanScheduleRuntime) ProcessDue(ctx context.Context) (result EvaluationPlanScheduleRunResult, err error) {
	if r == nil || !r.options.Enabled || r.store == nil || r.creator == nil {
		return result, nil
	}
	if !r.running.CompareAndSwap(false, true) {
		return result, nil
	}
	defer r.running.Store(false)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	now := r.nowTime()
	claims, err := r.store.ClaimDueScheduledPlans(ctx, now, r.options.ClaimBatch, r.options.LeaseDuration)
	if err != nil {
		return result, fmt.Errorf("claim due evaluation plans: %w", err)
	}
	result.Selected = len(claims)

	failures := make([]error, 0)
	for _, claim := range claims {
		started, runErr := r.runClaim(ctx, claim, now)
		switch {
		case runErr != nil:
			result.Failed++
			failures = append(failures, runErr)
		case started:
			result.Started++
		default:
			result.Skipped++
		}
	}
	return result, errors.Join(failures...)
}

// runClaim starts at most one run for a due plan, then always advances the
// schedule. Advancing on failure prevents a tight retry loop that would burn
// budget against a persistently failing plan.
func (r *EvaluationPlanScheduleRuntime) runClaim(ctx context.Context, claim ScheduledEvaluationPlanClaim, now time.Time) (bool, error) {
	baseline, candidate, refOK := evaluationPlanClaimReferences(claim)
	nextRunAt, cronErr := NextEvaluationPlanRun(claim.CronExpression, now)
	started := false
	var runErr error
	switch {
	case cronErr != nil:
		// An unparseable schedule cannot advance itself; the caller pushes a
		// full lease ahead so the plan is retried later instead of spinning.
		runErr = nil
	case !refOK:
		runErr = nil
	default:
		runCtx := ctx
		if claim.TenantID > 0 {
			runCtx = WithRadarTenant(runCtx, claim.TenantID)
		}
		if _, err := r.creator.CreateRunWithMatrix(runCtx, CreateRunInput{
			PlanID:        claim.PlanID,
			TriggerSource: "cron",
			BaselineRef:   baseline,
			CandidateRef:  candidate,
			CreatedBy:     claim.CreatedBy,
		}); err != nil {
			runErr = fmt.Errorf("create scheduled evaluation run for plan %s: %w", claim.PlanID, err)
		} else {
			started = true
		}
	}

	if cronErr != nil {
		nextRunAt = now.Add(r.options.LeaseDuration)
	}
	if err := r.store.CompleteScheduledPlanClaim(ctx, claim.PlanID, claim.LeaseToken, now, nextRunAt); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("complete scheduled evaluation plan %s: %w", claim.PlanID, err))
	}
	return started, runErr
}

func evaluationPlanClaimReferences(claim ScheduledEvaluationPlanClaim) (map[string]any, map[string]any, bool) {
	baseline, baselineOK := decodeEvaluationPlanReference(claim.BaselineRef)
	candidate, candidateOK := decodeEvaluationPlanReference(claim.CandidateRef)
	return baseline, candidate, baselineOK && candidateOK
}

func decodeEvaluationPlanReference(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	if len(value) == 0 {
		return nil, false
	}
	return value, true
}

func (r *EvaluationPlanScheduleRuntime) nowTime() time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return time.Now()
}

func logEvaluationPlanScheduleError(err error) {
	if err == nil {
		return
	}
	logger.LegacyPrintf("service.evaluation_plan_schedule", "[EvaluationPlanSchedule] tick failed: %v", err)
}
