package service

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	defaultEvaluationOutboxPollInterval      = 2 * time.Second
	defaultEvaluationOutboxClaimBatch        = 16
	defaultEvaluationOutboxMaxConcurrency    = 4
	defaultEvaluationOutboxLeaseDuration     = 60 * time.Second
	defaultEvaluationOutboxHeartbeatInterval = 20 * time.Second
	defaultEvaluationOutboxHandlerTimeout    = 45 * time.Second
	defaultEvaluationOutboxWorkerName        = "radar-control-plane-outbox"
	evaluationOutboxConsumerTimerName        = "radar:evaluation-outbox-consumer"
)

var evaluationOutboxEventTypes = []string{
	"route_evidence_sealed", "cell_recompute", "global_recompute", "gate_reevaluation",
}

var outboxTransientBackoff = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
	16 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second,
}

type EvaluationOutboxConsumerRuntimeOptions struct {
	PollInterval      time.Duration
	ClaimBatch        int
	MaxConcurrency    int
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	HandlerTimeout    time.Duration
	WorkerName        string
	Mode              EvaluationOutboxConsumerMode
}

type EvaluationOutboxConsumerResult struct {
	Selected       int
	Claimed        int
	Completed      int
	Retried        int
	DeadLettered   int
	Fenced         int
	PollSuppressed bool
	ErrorCode      string
}

type EvaluationOutboxDispatchHandler interface {
	Dispatch(context.Context, EvaluationOutboxEvent) EvaluationOutboxDispatchResult
	AfterComplete(context.Context, EvaluationOutboxEvent) error
}

type EvaluationOutboxConsumerRuntime struct {
	repository EvaluationOutboxRepository
	dispatcher EvaluationOutboxDispatchHandler
	options    EvaluationOutboxConsumerRuntimeOptions
	scheduler  RouteEvidenceTerminalizationScheduler

	workerMu sync.Mutex
	workerID uuid.UUID

	polling atomic.Bool

	lifecycleMu sync.Mutex
	started     bool
	cancel      context.CancelFunc
	runtimeWG   sync.WaitGroup
}

func NewEvaluationOutboxConsumerRuntime(repository EvaluationOutboxRepository, dispatcher EvaluationOutboxDispatchHandler, options EvaluationOutboxConsumerRuntimeOptions) *EvaluationOutboxConsumerRuntime {
	if options.PollInterval <= 0 {
		options.PollInterval = defaultEvaluationOutboxPollInterval
	}
	if options.ClaimBatch <= 0 {
		options.ClaimBatch = defaultEvaluationOutboxClaimBatch
	}
	if options.MaxConcurrency <= 0 {
		options.MaxConcurrency = defaultEvaluationOutboxMaxConcurrency
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = defaultEvaluationOutboxLeaseDuration
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = defaultEvaluationOutboxHeartbeatInterval
	}
	if options.HandlerTimeout <= 0 {
		options.HandlerTimeout = defaultEvaluationOutboxHandlerTimeout
	}
	if options.WorkerName == "" {
		options.WorkerName = defaultEvaluationOutboxWorkerName
	}
	return &EvaluationOutboxConsumerRuntime{repository: repository, dispatcher: dispatcher, options: options}
}

func (r *EvaluationOutboxConsumerRuntime) SetScheduler(scheduler RouteEvidenceTerminalizationScheduler) {
	if r != nil {
		r.scheduler = scheduler
	}
}

func (r *EvaluationOutboxConsumerRuntime) Start() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	if r.started {
		r.lifecycleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.started = true
	r.cancel = cancel
	scheduler := r.scheduler
	if scheduler == nil {
		r.runtimeWG.Add(1)
	}
	r.lifecycleMu.Unlock()
	if scheduler != nil {
		scheduler.ScheduleRecurring(evaluationOutboxConsumerTimerName, r.options.PollInterval, func() {
			r.triggerScheduled(ctx)
		})
		r.triggerScheduled(ctx)
		return
	}
	go r.runLoop(ctx)
}

func (r *EvaluationOutboxConsumerRuntime) runLoop(ctx context.Context) {
	defer r.runtimeWG.Done()
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()
	for {
		r.ProcessPending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *EvaluationOutboxConsumerRuntime) Stop() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	cancel := r.cancel
	scheduler := r.scheduler
	r.cancel = nil
	r.started = false
	r.lifecycleMu.Unlock()
	if scheduler != nil {
		scheduler.Cancel(evaluationOutboxConsumerTimerName)
	}
	if cancel != nil {
		cancel()
	}
	r.runtimeWG.Wait()
}

func (r *EvaluationOutboxConsumerRuntime) triggerScheduled(ctx context.Context) {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	if !r.started || ctx.Err() != nil {
		r.lifecycleMu.Unlock()
		return
	}
	r.runtimeWG.Add(1)
	r.lifecycleMu.Unlock()
	go func() {
		defer r.runtimeWG.Done()
		r.ProcessPending(ctx)
	}()
}

func (r *EvaluationOutboxConsumerRuntime) ProcessPending(ctx context.Context) EvaluationOutboxConsumerResult {
	var result EvaluationOutboxConsumerResult
	if r == nil || r.repository == nil || r.dispatcher == nil {
		result.ErrorCode = "runtime_unavailable"
		return result
	}
	if !r.polling.CompareAndSwap(false, true) {
		result.PollSuppressed = true
		return result
	}
	defer r.polling.Store(false)
	if err := ctx.Err(); err != nil {
		result.ErrorCode = "context_canceled"
		return result
	}
	workerID, err := r.ensureWorker(ctx)
	if err != nil {
		result.ErrorCode = "worker_registration_failed"
		return result
	}
	if err := r.touchWorkerHeartbeat(ctx, workerID); err != nil {
		result.ErrorCode = "worker_heartbeat_failed"
		return result
	}
	events, err := r.repository.Claim(ctx, workerID, evaluationOutboxEventTypes, r.options.ClaimBatch, r.options.LeaseDuration)
	if err != nil {
		result.ErrorCode = "claim_failed"
		return result
	}
	result.Selected = len(events)
	result.Claimed = len(events)
	if len(events) == 0 {
		return result
	}

	semaphore := make(chan struct{}, r.options.MaxConcurrency)
	var handlers sync.WaitGroup
	var counters runtimeCounters
	for _, event := range events {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}
		handlers.Add(1)
		go func(event EvaluationOutboxEvent) {
			defer handlers.Done()
			defer func() { <-semaphore }()
			r.processEvent(ctx, event, &counters)
		}(event)
	}
	handlers.Wait()
	result.Completed = int(counters.completed.Load())
	result.Retried = int(counters.retried.Load())
	result.DeadLettered = int(counters.deadLettered.Load())
	result.Fenced = int(counters.fenced.Load())
	return result
}

type runtimeCounters struct {
	completed    atomic.Int32
	retried      atomic.Int32
	deadLettered atomic.Int32
	fenced       atomic.Int32
}

func (r *EvaluationOutboxConsumerRuntime) processEvent(parent context.Context, event EvaluationOutboxEvent, counters *runtimeCounters) {
	handlerCtx, cancelHandler := context.WithTimeout(parent, r.options.HandlerTimeout)
	defer cancelHandler()

	var fenced atomic.Bool
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(r.options.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-handlerCtx.Done():
				return
			case <-ticker.C:
				if err := r.repository.Heartbeat(parent, event.ID, event.LeaseToken, event.LeaseEpoch, r.options.LeaseDuration); err != nil {
					fenced.Store(true)
					cancelHandler()
					return
				}
			}
		}
	}()

	dispatchResult := r.dispatcher.Dispatch(handlerCtx, event)
	cancelHandler()
	<-heartbeatDone
	if fenced.Load() {
		counters.fenced.Add(1)
		return
	}
	if parent.Err() != nil {
		return
	}
	if errors.Is(handlerCtx.Err(), context.DeadlineExceeded) {
		r.applyRetry(parent, event, EvaluationOutboxDispatchResult{
			Disposition: EvaluationOutboxDispatchRetry, ErrorCode: "handler_timeout",
		}, counters)
		return
	}
	r.applyDispatchResult(parent, event, dispatchResult, counters)
}

func (r *EvaluationOutboxConsumerRuntime) applyDispatchResult(ctx context.Context, event EvaluationOutboxEvent, result EvaluationOutboxDispatchResult, counters *runtimeCounters) {
	switch result.Disposition {
	case EvaluationOutboxDispatchComplete:
		if err := r.repository.Complete(ctx, event.ID, event.LeaseToken, event.LeaseEpoch); err != nil {
			if errors.Is(err, ErrEvaluationOutboxFenced) || errors.Is(err, ErrRadarForbidden) {
				counters.fenced.Add(1)
			}
			return
		}
		counters.completed.Add(1)
		// Completion is durable, so shutdown must not abandon final Run reconciliation.
		afterCompleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.options.HandlerTimeout)
		defer cancel()
		_ = r.dispatcher.AfterComplete(afterCompleteCtx, event)
	case EvaluationOutboxDispatchRetry:
		r.applyRetry(ctx, event, result, counters)
	case EvaluationOutboxDispatchDeadLetter:
		if err := r.repository.DeadLetter(ctx, event.ID, event.LeaseToken, event.LeaseEpoch, result.ErrorCode); err != nil {
			if errors.Is(err, ErrEvaluationOutboxFenced) || errors.Is(err, ErrRadarForbidden) {
				counters.fenced.Add(1)
			}
			return
		}
		counters.deadLettered.Add(1)
	case EvaluationOutboxDispatchFenced:
		counters.fenced.Add(1)
	}
}

func (r *EvaluationOutboxConsumerRuntime) applyRetry(ctx context.Context, event EvaluationOutboxEvent, result EvaluationOutboxDispatchResult, counters *runtimeCounters) {
	code := result.ErrorCode
	if code == "" {
		code = "outbox_handler_failed"
	}
	delay := result.RetryAfter
	if code == "aggregate_dependency_pending" {
		if !event.CreatedAt.IsZero() && time.Since(event.CreatedAt) >= 24*time.Hour {
			if err := r.repository.DeadLetter(ctx, event.ID, event.LeaseToken, event.LeaseEpoch, "aggregate_dependency_timeout"); err == nil {
				counters.deadLettered.Add(1)
			} else if errors.Is(err, ErrEvaluationOutboxFenced) || errors.Is(err, ErrRadarForbidden) {
				counters.fenced.Add(1)
			}
			return
		}
		if delay <= 0 {
			delay = aggregateDependencyBackoff(event.Attempt)
		}
	} else if code != "gate_full_mode_required" && event.Attempt >= len(outboxTransientBackoff) {
		if err := r.repository.DeadLetter(ctx, event.ID, event.LeaseToken, event.LeaseEpoch, code); err == nil {
			counters.deadLettered.Add(1)
		} else if errors.Is(err, ErrEvaluationOutboxFenced) || errors.Is(err, ErrRadarForbidden) {
			counters.fenced.Add(1)
		}
		return
	} else if delay <= 0 {
		delay = transientOutboxBackoff(event.Attempt)
	}
	if err := r.repository.Retry(ctx, event.ID, event.LeaseToken, event.LeaseEpoch, code, delay); err != nil {
		if errors.Is(err, ErrEvaluationOutboxFenced) || errors.Is(err, ErrRadarForbidden) {
			counters.fenced.Add(1)
		}
		return
	}
	counters.retried.Add(1)
}

func aggregateDependencyBackoff(attempt int) time.Duration {
	attempt--
	if attempt < 0 {
		attempt = 0
	}
	exponent := math.Min(float64(attempt), 5)
	delay := 2 * time.Second * time.Duration(1<<int(exponent))
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

func transientOutboxBackoff(attempt int) time.Duration {
	attempt--
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(outboxTransientBackoff) {
		attempt = len(outboxTransientBackoff) - 1
	}
	return outboxTransientBackoff[attempt]
}

func (r *EvaluationOutboxConsumerRuntime) ensureWorker(ctx context.Context) (uuid.UUID, error) {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	if r.workerID != uuid.Nil {
		return r.workerID, nil
	}
	workerID, err := r.repository.EnsureConsumerWorker(ctx, r.options.WorkerName)
	if err != nil {
		return uuid.Nil, err
	}
	r.workerID = workerID
	return workerID, nil
}

func (r *EvaluationOutboxConsumerRuntime) touchWorkerHeartbeat(ctx context.Context, workerID uuid.UUID) error {
	err := r.repository.TouchConsumerWorkerHeartbeat(ctx, workerID)
	if err != nil {
		r.workerMu.Lock()
		if r.workerID == workerID {
			r.workerID = uuid.Nil
		}
		r.workerMu.Unlock()
	}
	return err
}
