package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type runtimeRetryCall struct {
	eventID uuid.UUID
	code    string
	delay   time.Duration
}

type outboxRuntimeRepositoryStub struct {
	mu                     sync.Mutex
	workerID               uuid.UUID
	events                 []EvaluationOutboxEvent
	claimCalls             int
	heartbeatCalls         int
	consumerHeartbeatCalls int
	completeIDs            []uuid.UUID
	deadLetters            []string
	retries                []runtimeRetryCall
	heartbeatFn            func(int) error
	claimFn                func(context.Context) ([]EvaluationOutboxEvent, error)
}

func (s *outboxRuntimeRepositoryStub) Enqueue(context.Context, EnqueueEvaluationOutboxInput) (*EvaluationOutboxEvent, error) {
	return nil, errors.New("not implemented")
}

func (s *outboxRuntimeRepositoryStub) Claim(ctx context.Context, _ uuid.UUID, _ []string, _ int, _ time.Duration) ([]EvaluationOutboxEvent, error) {
	s.mu.Lock()
	s.claimCalls++
	claimFn := s.claimFn
	if claimFn != nil {
		s.mu.Unlock()
		return claimFn(ctx)
	}
	events := append([]EvaluationOutboxEvent(nil), s.events...)
	s.events = nil
	s.mu.Unlock()
	return events, nil
}

func (s *outboxRuntimeRepositoryStub) Heartbeat(_ context.Context, _ uuid.UUID, _ string, _ int64, _ time.Duration) error {
	s.mu.Lock()
	s.heartbeatCalls++
	count := s.heartbeatCalls
	hook := s.heartbeatFn
	s.mu.Unlock()
	if hook != nil {
		return hook(count)
	}
	return nil
}

func (s *outboxRuntimeRepositoryStub) Complete(_ context.Context, eventID uuid.UUID, _ string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeIDs = append(s.completeIDs, eventID)
	return nil
}

func (s *outboxRuntimeRepositoryStub) Retry(_ context.Context, eventID uuid.UUID, _ string, _ int64, code string, delay time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries = append(s.retries, runtimeRetryCall{eventID: eventID, code: code, delay: delay})
	return nil
}

func (s *outboxRuntimeRepositoryStub) DeadLetter(_ context.Context, eventID uuid.UUID, _ string, _ int64, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadLetters = append(s.deadLetters, eventID.String()+":"+code)
	return nil
}

func (s *outboxRuntimeRepositoryStub) ReplayDeadLetter(context.Context, uuid.UUID) (*EvaluationOutboxEvent, error) {
	return nil, errors.New("not implemented")
}

func (s *outboxRuntimeRepositoryStub) EnsureConsumerWorker(context.Context, string) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workerID == uuid.Nil {
		s.workerID = uuid.New()
	}
	return s.workerID, nil
}

func (s *outboxRuntimeRepositoryStub) TouchConsumerWorkerHeartbeat(context.Context, uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consumerHeartbeatCalls++
	return nil
}

func (s *outboxRuntimeRepositoryStub) heartbeatCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heartbeatCalls
}

func (s *outboxRuntimeRepositoryStub) consumerHeartbeatCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumerHeartbeatCalls
}

type outboxRuntimeDispatcherStub struct {
	dispatchFn      func(context.Context, EvaluationOutboxEvent) EvaluationOutboxDispatchResult
	afterCompleteFn func(context.Context, EvaluationOutboxEvent) error
	active          atomic.Int32
	maxActive       atomic.Int32
}

type outboxRuntimeSchedulerStub struct {
	name     string
	canceled string
}

func (s *outboxRuntimeSchedulerStub) ScheduleRecurring(name string, _ time.Duration, _ func()) {
	s.name = name
}

func (s *outboxRuntimeSchedulerStub) Cancel(name string) {
	s.canceled = name
}

func (s *outboxRuntimeDispatcherStub) Dispatch(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	active := s.active.Add(1)
	for {
		max := s.maxActive.Load()
		if active <= max || s.maxActive.CompareAndSwap(max, active) {
			break
		}
	}
	defer s.active.Add(-1)
	if s.dispatchFn != nil {
		return s.dispatchFn(ctx, event)
	}
	return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchComplete}
}

func (s *outboxRuntimeDispatcherStub) AfterComplete(ctx context.Context, event EvaluationOutboxEvent) error {
	if s.afterCompleteFn != nil {
		return s.afterCompleteFn(ctx, event)
	}
	return nil
}

func runtimeEvent() EvaluationOutboxEvent {
	return EvaluationOutboxEvent{
		ID: uuid.New(), RunID: uuid.New(), EventType: "cell_recompute", SourceType: "score_head_event",
		SourceID: uuid.NewString(), SourceHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PayloadHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Payload:     []byte(`{"capability_domain":"coding","model_route":"route-a","score_head_event_id":"00000000-0000-0000-0000-000000000001"}`),
		LeaseToken:  "lease-token", LeaseEpoch: 1, CreatedAt: time.Now().UTC(), Attempt: 1,
	}
}

func testOutboxRuntimeOptions() EvaluationOutboxConsumerRuntimeOptions {
	return EvaluationOutboxConsumerRuntimeOptions{
		PollInterval:      time.Hour,
		ClaimBatch:        16,
		MaxConcurrency:    4,
		LeaseDuration:     time.Second,
		HeartbeatInterval: 5 * time.Millisecond,
		HandlerTimeout:    time.Second,
		WorkerName:        "test-outbox",
		Mode:              EvaluationOutboxConsumerModeCore,
	}
}

func TestProvideEvaluationOutboxConsumerRuntimeStartsOnlyForEnabledModes(t *testing.T) {
	tests := []struct {
		name      string
		enabled   bool
		mode      string
		wantStart bool
	}{
		{name: "radar disabled", enabled: false, mode: "core", wantStart: false},
		{name: "consumer disabled", enabled: true, mode: "disabled", wantStart: false},
		{name: "core", enabled: true, mode: "core", wantStart: true},
		{name: "full", enabled: true, mode: "full", wantStart: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheduler := &outboxRuntimeSchedulerStub{}
			runtime := ProvideEvaluationOutboxConsumerRuntime(
				&outboxRuntimeRepositoryStub{},
				&outboxRuntimeDispatcherStub{},
				scheduler,
				&config.Config{Radar: config.RadarConfig{
					Enabled: test.enabled, OutboxConsumerMode: test.mode,
				}},
			)

			if test.wantStart {
				require.Equal(t, evaluationOutboxConsumerTimerName, scheduler.name)
			} else {
				require.Empty(t, scheduler.name)
			}
			runtime.Stop()
			if test.wantStart {
				require.Equal(t, evaluationOutboxConsumerTimerName, scheduler.canceled)
			}
		})
	}
}

func TestOutboxRuntimeHeartbeatsUntilHandlerCompletes(t *testing.T) {
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{runtimeEvent()}}
	started := make(chan struct{})
	release := make(chan struct{})
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(ctx context.Context, _ EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		close(started)
		select {
		case <-release:
			return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchComplete}
		case <-ctx.Done():
			return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchRetry, ErrorCode: "handler_canceled"}
		}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())

	done := make(chan EvaluationOutboxConsumerResult, 1)
	go func() { done <- runtime.ProcessPending(context.Background()) }()
	<-started
	require.Eventually(t, func() bool { return repo.heartbeatCount() >= 2 }, time.Second, time.Millisecond)
	close(release)
	result := <-done

	require.Equal(t, 1, result.Completed)
	require.Len(t, repo.completeIDs, 1)
}

func TestOutboxRuntimeTouchesConsumerWorkerHeartbeatWhenQueueEmpty(t *testing.T) {
	repo := &outboxRuntimeRepositoryStub{}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, &outboxRuntimeDispatcherStub{}, testOutboxRuntimeOptions())

	result := runtime.ProcessPending(context.Background())

	require.Empty(t, result.ErrorCode)
	require.Equal(t, 1, repo.consumerHeartbeatCount())
}

func TestOutboxRuntimeNeverExceedsConfiguredConcurrency(t *testing.T) {
	events := make([]EvaluationOutboxEvent, 8)
	for i := range events {
		events[i] = runtimeEvent()
	}
	repo := &outboxRuntimeRepositoryStub{events: events}
	started := make(chan struct{}, len(events))
	release := make(chan struct{})
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(ctx context.Context, _ EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		started <- struct{}{}
		select {
		case <-release:
			return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchComplete}
		case <-ctx.Done():
			return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchFenced}
		}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())
	done := make(chan EvaluationOutboxConsumerResult, 1)
	go func() { done <- runtime.ProcessPending(context.Background()) }()
	require.Eventually(t, func() bool { return len(started) == 4 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(4), dispatcher.maxActive.Load())
	close(release)
	result := <-done
	require.Equal(t, 8, result.Completed)
}

func TestOutboxRuntimeRetriesAggregateDependencyWithBackoff(t *testing.T) {
	event := runtimeEvent()
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{event}}
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(context.Context, EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchRetry, ErrorCode: "aggregate_dependency_pending"}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())

	result := runtime.ProcessPending(context.Background())

	require.Equal(t, 1, result.Retried)
	require.Len(t, repo.retries, 1)
	require.Equal(t, "aggregate_dependency_pending", repo.retries[0].code)
	require.Equal(t, 2*time.Second, repo.retries[0].delay)
}

func TestOutboxRuntimeDeadLettersExpiredAggregateDependency(t *testing.T) {
	event := runtimeEvent()
	event.CreatedAt = time.Now().Add(-24 * time.Hour)
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{event}}
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(context.Context, EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchRetry, ErrorCode: "aggregate_dependency_pending"}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())

	result := runtime.ProcessPending(context.Background())

	require.Equal(t, 1, result.DeadLettered)
	require.Equal(t, event.ID.String()+":aggregate_dependency_timeout", repo.deadLetters[0])
	require.Empty(t, repo.retries)
}

func TestOutboxRuntimeDeadLettersEighthTransientAttempt(t *testing.T) {
	event := runtimeEvent()
	event.Attempt = 8
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{event}}
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(context.Context, EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchRetry, ErrorCode: "outbox_handler_failed"}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())

	result := runtime.ProcessPending(context.Background())

	require.Equal(t, 1, result.DeadLettered)
	require.Equal(t, event.ID.String()+":outbox_handler_failed", repo.deadLetters[0])
	require.Empty(t, repo.retries)
}

func TestOutboxRuntimeCoreGateWaitDoesNotExhaustAttempts(t *testing.T) {
	event := runtimeEvent()
	event.Attempt = 100
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{event}}
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(context.Context, EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		return EvaluationOutboxDispatchResult{
			Disposition: EvaluationOutboxDispatchRetry, ErrorCode: "gate_full_mode_required", RetryAfter: time.Minute,
		}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())

	result := runtime.ProcessPending(context.Background())

	require.Equal(t, 1, result.Retried)
	require.Equal(t, time.Minute, repo.retries[0].delay)
	require.Empty(t, repo.deadLetters)
}

func TestOutboxRuntimeDeadLettersPermanentDispatchFailure(t *testing.T) {
	event := runtimeEvent()
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{event}}
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(context.Context, EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchDeadLetter, ErrorCode: "payload_schema_invalid"}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())

	result := runtime.ProcessPending(context.Background())

	require.Equal(t, 1, result.DeadLettered)
	require.Equal(t, event.ID.String()+":payload_schema_invalid", repo.deadLetters[0])
}

func TestOutboxRuntimeHeartbeatFencingCancelsHandlerWithoutStateUpdate(t *testing.T) {
	event := runtimeEvent()
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{event}, heartbeatFn: func(count int) error {
		if count >= 2 {
			return ErrEvaluationOutboxFenced
		}
		return nil
	}}
	started := make(chan struct{})
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(ctx context.Context, _ EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		close(started)
		<-ctx.Done()
		return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchComplete}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())

	done := make(chan EvaluationOutboxConsumerResult, 1)
	go func() { done <- runtime.ProcessPending(context.Background()) }()
	<-started
	result := <-done

	require.Equal(t, 1, result.Fenced)
	require.Empty(t, repo.completeIDs)
	require.Empty(t, repo.retries)
	require.Empty(t, repo.deadLetters)
}

func TestOutboxRuntimeCancellationLeavesLeaseUntouched(t *testing.T) {
	event := runtimeEvent()
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{event}}
	started := make(chan struct{})
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(ctx context.Context, _ EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		close(started)
		<-ctx.Done()
		return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchRetry, ErrorCode: "handler_canceled"}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan EvaluationOutboxConsumerResult, 1)
	go func() { done <- runtime.ProcessPending(ctx) }()
	<-started
	cancel()
	result := <-done

	require.Equal(t, 0, result.Completed)
	require.Empty(t, repo.completeIDs)
	require.Empty(t, repo.retries)
	require.Empty(t, repo.deadLetters)
}

func TestOutboxRuntimeSuppressesOverlappingPolls(t *testing.T) {
	claimStarted := make(chan struct{})
	releaseClaim := make(chan struct{})
	repo := &outboxRuntimeRepositoryStub{claimFn: func(ctx context.Context) ([]EvaluationOutboxEvent, error) {
		close(claimStarted)
		select {
		case <-releaseClaim:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, &outboxRuntimeDispatcherStub{}, testOutboxRuntimeOptions())
	firstDone := make(chan EvaluationOutboxConsumerResult, 1)
	go func() { firstDone <- runtime.ProcessPending(context.Background()) }()
	<-claimStarted

	second := runtime.ProcessPending(context.Background())

	require.True(t, second.PollSuppressed)
	close(releaseClaim)
	<-firstDone
}

func TestOutboxRuntimeStopWaitsForActiveHandler(t *testing.T) {
	event := runtimeEvent()
	repo := &outboxRuntimeRepositoryStub{events: []EvaluationOutboxEvent{event}}
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	dispatcher := &outboxRuntimeDispatcherStub{dispatchFn: func(ctx context.Context, _ EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
		close(handlerStarted)
		<-ctx.Done()
		<-releaseHandler
		return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchRetry, ErrorCode: "handler_canceled"}
	}}
	runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testOutboxRuntimeOptions())
	runtime.Start()
	<-handlerStarted
	stopped := make(chan struct{})
	go func() { runtime.Stop(); close(stopped) }()

	select {
	case <-stopped:
		t.Fatal("Stop returned before the active handler exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseHandler)
	require.Eventually(t, func() bool {
		select {
		case <-stopped:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.Empty(t, repo.completeIDs)
	require.Empty(t, repo.retries)
	require.Empty(t, repo.deadLetters)
}
