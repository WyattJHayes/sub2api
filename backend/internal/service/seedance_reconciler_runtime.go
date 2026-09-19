package service

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	seedanceReconcilerTimerName = "gateway:seedance-reconciler"

	defaultSeedanceReconcilerPollInterval   = 5 * time.Second
	defaultSeedanceReconcilerRequestTimeout = 20 * time.Second
	defaultSeedanceReconcilerLeaseDuration  = time.Minute
	defaultSeedanceReconcilerNotFoundGrace  = time.Minute
	defaultSeedanceReconcilerClaimBatch     = 8
	defaultSeedanceReconcilerMaxConcurrency = 1

	maxSeedanceReconcilerClaimBatch     = 64
	maxSeedanceReconcilerMaxConcurrency = 4
)

var seedanceRetrySequence = []time.Duration{
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
	time.Minute,
}

type SeedanceReconcilerOptions struct {
	Enabled        bool
	PollInterval   time.Duration
	RequestTimeout time.Duration
	LeaseDuration  time.Duration
	NotFoundGrace  time.Duration
	ClaimBatch     int
	MaxConcurrency int
}

type SeedanceReconcilerRunResult struct {
	Selected     int
	Settled      int
	Retried      int
	Terminal     int
	DeadLettered int
	Fenced       int
}

type SeedanceReconcilerRuntime struct {
	tasks      AsyncVideoBillingTaskRepository
	accounts   AccountRepository
	client     SeedanceTaskClient
	settlement *SeedanceTaskSettlementService
	options    SeedanceReconcilerOptions
	scheduler  RouteEvidenceTerminalizationScheduler
	log        *zap.Logger

	running atomic.Bool

	lifecycleMu sync.Mutex
	started     bool
	generation  uint64
	cancel      context.CancelFunc
	runtimeWG   sync.WaitGroup

	now    func() time.Time
	jitter func() float64
}

func NewSeedanceReconcilerRuntime(
	tasks AsyncVideoBillingTaskRepository,
	accounts AccountRepository,
	client SeedanceTaskClient,
	settlement *SeedanceTaskSettlementService,
	options SeedanceReconcilerOptions,
) *SeedanceReconcilerRuntime {
	return &SeedanceReconcilerRuntime{
		tasks:      tasks,
		accounts:   accounts,
		client:     client,
		settlement: settlement,
		options:    normalizeSeedanceReconcilerOptions(options),
		log:        logger.With(zap.String("component", "service.seedance_reconciler")),
		now:        time.Now,
		jitter: func() float64 {
			return rand.Float64()*0.2 - 0.1
		},
	}
}

func (r *SeedanceReconcilerRuntime) SetScheduler(scheduler RouteEvidenceTerminalizationScheduler) {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	r.scheduler = scheduler
	r.lifecycleMu.Unlock()
}

func (r *SeedanceReconcilerRuntime) Start() {
	if r == nil || !r.options.Enabled || r.tasks == nil || r.accounts == nil || r.client == nil || r.settlement == nil {
		return
	}
	r.lifecycleMu.Lock()
	if r.started || r.scheduler == nil {
		r.lifecycleMu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	r.started = true
	r.generation++
	generation := r.generation
	r.cancel = cancel
	scheduler := r.scheduler
	interval := r.options.PollInterval
	scheduler.ScheduleRecurring(seedanceReconcilerTimerName, interval, func() {
		r.trigger(workerCtx, generation)
	})
	r.lifecycleMu.Unlock()
	r.trigger(workerCtx, generation)
}

func (r *SeedanceReconcilerRuntime) Stop() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	if !r.started {
		r.lifecycleMu.Unlock()
		return
	}
	r.started = false
	r.generation++
	cancel := r.cancel
	r.cancel = nil
	scheduler := r.scheduler
	if scheduler != nil {
		scheduler.Cancel(seedanceReconcilerTimerName)
	}
	if cancel != nil {
		cancel()
	}
	r.runtimeWG.Wait()
	r.lifecycleMu.Unlock()
}

func (r *SeedanceReconcilerRuntime) ProcessDue(ctx context.Context) (result SeedanceReconcilerRunResult, err error) {
	started := time.Now()
	defer func() {
		logSeedanceReconcilerRun(r.operationLogger(), result, err, time.Since(started))
	}()
	if r == nil || r.tasks == nil || r.accounts == nil || r.client == nil || r.settlement == nil {
		return result, errors.New("seedance reconciler runtime is unavailable")
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
	tasks, err := r.tasks.ClaimDue(
		ctx,
		AsyncVideoBillingProviderSeedance,
		now,
		r.options.ClaimBatch,
		r.options.LeaseDuration,
	)
	if err != nil {
		return result, fmt.Errorf("claim due seedance billing tasks: %w", err)
	}
	result.Selected = len(tasks)
	if len(tasks) == 0 {
		return result, nil
	}

	semaphore := make(chan struct{}, r.options.MaxConcurrency)
	var wg sync.WaitGroup
	var resultMu sync.Mutex
	failures := make([]error, 0)
	appendFailure := func(err error) {
		if err == nil {
			return
		}
		resultMu.Lock()
		failures = append(failures, err)
		resultMu.Unlock()
	}
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			appendFailure(err)
			break
		}
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			appendFailure(ctx.Err())
			break
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(task AsyncVideoBillingTask) {
			defer wg.Done()
			defer func() { <-semaphore }()
			taskResult, taskErr := r.processTask(ctx, task)
			resultMu.Lock()
			mergeSeedanceReconcilerResult(&result, taskResult)
			if taskErr != nil {
				failures = append(failures, taskErr)
			}
			resultMu.Unlock()
		}(task)
	}
	wg.Wait()
	return result, errors.Join(failures...)
}

func (r *SeedanceReconcilerRuntime) trigger(ctx context.Context, generation uint64) {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	if !r.started || r.generation != generation || ctx.Err() != nil {
		r.lifecycleMu.Unlock()
		return
	}
	r.runtimeWG.Add(1)
	r.lifecycleMu.Unlock()

	go func() {
		defer r.runtimeWG.Done()
		_, _ = r.ProcessDue(ctx)
	}()
}

func (r *SeedanceReconcilerRuntime) processTask(parent context.Context, task AsyncVideoBillingTask) (SeedanceReconcilerRunResult, error) {
	var result SeedanceReconcilerRunResult
	started := time.Now()
	r.logTaskEvent(zap.InfoLevel, "seedance_task_claimed", task, task.Status, "", time.Since(started))
	requestCtx, cancel := context.WithTimeout(parent, r.options.RequestTimeout)
	defer cancel()

	account, err := r.accounts.GetByID(requestCtx, task.AccountID)
	if err != nil || account == nil {
		if requestCtx.Err() != nil && parent.Err() != nil {
			return result, parent.Err()
		}
		writeCtx := requestCtx
		if requestCtx.Err() != nil {
			writeCtx = context.WithoutCancel(parent)
		}
		if errors.Is(err, ErrAccountNotFound) || (err == nil && account == nil) {
			return r.deadLetter(writeCtx, task, "seedance_account_missing", started)
		}
		return r.retry(writeCtx, task, "seedance_account_load_failed", 0, started)
	}

	observed, err := r.client.GetSeedanceTask(requestCtx, account, task.UpstreamTaskID)
	if err != nil {
		if parent.Err() != nil {
			return result, parent.Err()
		}
		if requestCtx.Err() != nil {
			return r.retry(context.WithoutCancel(parent), task, "seedance_upstream_timeout", 0, started)
		}
		return r.handleUpstreamError(requestCtx, task, err, started)
	}
	if parent.Err() != nil {
		return result, parent.Err()
	}

	settled := r.settlement.ProcessClaimed(requestCtx, task, observed, r.nowTime())
	result.Settled = boolCount(settled.Settled)
	result.Retried = boolCount(settled.Retried)
	result.Terminal = boolCount(settled.Terminal)
	result.DeadLettered = boolCount(settled.DeadLettered)
	result.Fenced = boolCount(settled.Fenced)
	r.logSettlementResult(task, observed, settled, time.Since(started))
	if settled.Fenced {
		return result, nil
	}
	return result, settled.Err
}

func (r *SeedanceReconcilerRuntime) handleUpstreamError(ctx context.Context, task AsyncVideoBillingTask, err error, started time.Time) (SeedanceReconcilerRunResult, error) {
	var upstreamErr *SeedanceUpstreamError
	if !errors.As(err, &upstreamErr) {
		return r.retry(ctx, task, "seedance_upstream_temporary", 0, started)
	}
	switch upstreamErr.Kind {
	case SeedanceUpstreamErrorNotFound:
		if r.nowTime().Sub(task.CreatedAt) <= r.options.NotFoundGrace {
			return r.retry(ctx, task, "seedance_not_found_eventual", 0, started)
		}
		return r.deadLetter(ctx, task, "seedance_not_found_persistent", started)
	case SeedanceUpstreamErrorAuth:
		return r.deadLetter(ctx, task, "seedance_upstream_auth", started)
	case SeedanceUpstreamErrorProtocol:
		return r.deadLetter(ctx, task, "seedance_upstream_protocol", started)
	case SeedanceUpstreamErrorRateLimited:
		return r.retry(ctx, task, "seedance_upstream_rate_limited", upstreamErr.RetryDelay, started)
	case SeedanceUpstreamErrorTemporary:
		return r.retry(ctx, task, "seedance_upstream_temporary", upstreamErr.RetryDelay, started)
	default:
		return r.retry(ctx, task, "seedance_upstream_temporary", 0, started)
	}
}

func (r *SeedanceReconcilerRuntime) retry(ctx context.Context, task AsyncVideoBillingTask, errorCode string, minimumDelay time.Duration, started time.Time) (SeedanceReconcilerRunResult, error) {
	var result SeedanceReconcilerRunResult
	delay := seedanceRetryDelay(task.AttemptCount, r.jitterValue())
	if minimumDelay > delay {
		delay = minimumDelay
	}
	err := r.tasks.MarkRetry(ctx, seedanceTaskLease(task), r.nowTime().Add(delay), errorCode, task.MissingTokenChecks)
	if errors.Is(err, ErrAsyncVideoBillingTaskFenced) {
		result.Fenced = 1
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Retried = 1
	r.logTaskEvent(zap.WarnLevel, "seedance_task_retried", task, AsyncVideoBillingStatusPending, errorCode, time.Since(started))
	return result, nil
}

func (r *SeedanceReconcilerRuntime) deadLetter(ctx context.Context, task AsyncVideoBillingTask, errorCode string, started time.Time) (SeedanceReconcilerRunResult, error) {
	var result SeedanceReconcilerRunResult
	err := r.tasks.MarkTerminal(ctx, seedanceTaskLease(task), AsyncVideoBillingStatusDeadLetter, errorCode, r.nowTime())
	if errors.Is(err, ErrAsyncVideoBillingTaskFenced) {
		result.Fenced = 1
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.DeadLettered = 1
	r.logTaskEvent(zap.ErrorLevel, "seedance_task_dead_lettered", task, AsyncVideoBillingStatusDeadLetter, errorCode, time.Since(started))
	return result, nil
}

func seedanceRetryDelay(attempt int, jitter float64) time.Duration {
	base := 5 * time.Minute
	if attempt > 0 && attempt <= len(seedanceRetrySequence) {
		base = seedanceRetrySequence[attempt-1]
	}
	if jitter < -0.1 {
		jitter = -0.1
	}
	if jitter > 0.1 {
		jitter = 0.1
	}
	return time.Duration(float64(base) * (1 + jitter))
}

func normalizeSeedanceReconcilerOptions(options SeedanceReconcilerOptions) SeedanceReconcilerOptions {
	if options.PollInterval <= 0 {
		options.PollInterval = defaultSeedanceReconcilerPollInterval
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = defaultSeedanceReconcilerRequestTimeout
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = defaultSeedanceReconcilerLeaseDuration
	}
	if options.NotFoundGrace <= 0 {
		options.NotFoundGrace = defaultSeedanceReconcilerNotFoundGrace
	}
	if options.ClaimBatch <= 0 {
		options.ClaimBatch = defaultSeedanceReconcilerClaimBatch
	}
	if options.ClaimBatch > maxSeedanceReconcilerClaimBatch {
		options.ClaimBatch = maxSeedanceReconcilerClaimBatch
	}
	if options.MaxConcurrency <= 0 {
		options.MaxConcurrency = defaultSeedanceReconcilerMaxConcurrency
	}
	if options.MaxConcurrency > maxSeedanceReconcilerMaxConcurrency {
		options.MaxConcurrency = maxSeedanceReconcilerMaxConcurrency
	}
	return options
}

func (r *SeedanceReconcilerRuntime) nowTime() time.Time {
	if r == nil || r.now == nil {
		return time.Now()
	}
	return r.now()
}

func (r *SeedanceReconcilerRuntime) jitterValue() float64 {
	if r == nil || r.jitter == nil {
		return 0
	}
	return r.jitter()
}

func mergeSeedanceReconcilerResult(target *SeedanceReconcilerRunResult, source SeedanceReconcilerRunResult) {
	target.Settled += source.Settled
	target.Retried += source.Retried
	target.Terminal += source.Terminal
	target.DeadLettered += source.DeadLettered
	target.Fenced += source.Fenced
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (r *SeedanceReconcilerRuntime) operationLogger() *zap.Logger {
	if r != nil && r.log != nil {
		return r.log
	}
	return logger.With(zap.String("component", "service.seedance_reconciler"))
}

func (r *SeedanceReconcilerRuntime) logSettlementResult(
	task AsyncVideoBillingTask,
	observed *SeedanceUpstreamResponse,
	result SeedanceSettlementResult,
	elapsed time.Duration,
) {
	switch {
	case result.Settled:
		r.logTaskEvent(zap.InfoLevel, "seedance_task_settled", task, AsyncVideoBillingStatusSettled, result.ErrorCode, elapsed)
	case result.Retried:
		r.logTaskEvent(zap.WarnLevel, "seedance_task_retried", task, AsyncVideoBillingStatusPending, result.ErrorCode, elapsed)
	case result.DeadLettered:
		r.logTaskEvent(zap.ErrorLevel, "seedance_task_dead_lettered", task, AsyncVideoBillingStatusDeadLetter, result.ErrorCode, elapsed)
	case result.Terminal:
		status := AsyncVideoBillingStatusFailed
		if observed != nil && observed.State == SeedanceObservedCancelled {
			status = AsyncVideoBillingStatusCancelled
		}
		r.logTaskEvent(zap.InfoLevel, "seedance_task_terminal", task, status, result.ErrorCode, elapsed)
	}
}

func (r *SeedanceReconcilerRuntime) logTaskEvent(
	level zapcore.Level,
	event string,
	task AsyncVideoBillingTask,
	status string,
	errorCode string,
	elapsed time.Duration,
) {
	fields := []zap.Field{
		zap.String("provider", task.Provider),
		zap.String("task_id", task.TaskKey),
		zap.Int64("task_record_id", task.ID),
		zap.Int64("user_id", task.UserID),
		zap.Int64("api_key_id", task.APIKeyID),
		zap.Int64("account_id", task.AccountID),
		zap.Int("attempt_count", task.AttemptCount),
		zap.Int64("elapsed_ms", seedanceElapsedMillis(elapsed)),
		zap.String("status", status),
		zap.String("error_code", errorCode),
	}
	if task.GroupID != nil {
		fields = append(fields, zap.Int64("group_id", *task.GroupID))
	}
	if task.SubscriptionID != nil {
		fields = append(fields, zap.Int64("subscription_id", *task.SubscriptionID))
	}
	log := r.operationLogger()
	switch level {
	case zap.DebugLevel:
		log.Debug(event, fields...)
	case zap.WarnLevel:
		log.Warn(event, fields...)
	case zap.ErrorLevel:
		log.Error(event, fields...)
	default:
		log.Info(event, fields...)
	}
}

func logSeedanceReconcilerRun(log *zap.Logger, result SeedanceReconcilerRunResult, err error, elapsed time.Duration) {
	if log == nil {
		log = zap.NewNop()
	}
	fields := []zap.Field{
		zap.Int("selected", result.Selected),
		zap.Int("settled", result.Settled),
		zap.Int("retried", result.Retried),
		zap.Int("terminal", result.Terminal),
		zap.Int("dead_lettered", result.DeadLettered),
		zap.Int("fenced", result.Fenced),
		zap.Int64("elapsed_ms", seedanceElapsedMillis(elapsed)),
	}
	if err != nil {
		log.Error("seedance_reconciler_poll", append(fields,
			zap.String("status", "failed"),
			zap.String("error_code", "seedance_reconciler_failed"),
		)...)
		return
	}
	if result.Selected == 0 {
		log.Debug("seedance_reconciler_poll", append(fields,
			zap.String("status", "idle"),
			zap.String("error_code", ""),
		)...)
		return
	}
	log.Info("seedance_reconciler_poll", append(fields,
		zap.String("status", "completed"),
		zap.String("error_code", ""),
	)...)
}

func seedanceElapsedMillis(elapsed time.Duration) int64 {
	milliseconds := elapsed.Milliseconds()
	if milliseconds < 0 {
		return 0
	}
	return milliseconds
}
