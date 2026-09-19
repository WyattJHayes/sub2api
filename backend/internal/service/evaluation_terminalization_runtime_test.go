package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type terminalizationRuntimeRepositoryStub struct {
	mu        sync.Mutex
	events    []RouteEvidenceTerminalizationEvent
	listErr   error
	listCalls int
}

func (s *terminalizationRuntimeRepositoryStub) UpsertTransport(context.Context, RouteEvidence) error {
	return nil
}
func (s *terminalizationRuntimeRepositoryStub) AttachBilling(context.Context, string, RouteUsageEvidence) error {
	return nil
}

func (s *terminalizationRuntimeRepositoryStub) ListPendingTerminalizations(_ context.Context, _ int) ([]RouteEvidenceTerminalizationEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return append([]RouteEvidenceTerminalizationEvent(nil), s.events...), s.listErr
}

func (s *terminalizationRuntimeRepositoryStub) ListCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls
}

type terminalizationRuntimeFinalizerStub struct {
	mu        sync.Mutex
	sealed    map[uuid.UUID]int
	failures  map[uuid.UUID]int
	calls     []FinalizeRouteEvidenceFromTerminalizationInput
	called    chan struct{}
	block     bool
	cancelled chan struct{}
}

type terminalizationRuntimeContractStub struct {
	*terminalizationRuntimeRepositoryStub
	*terminalizationRuntimeFinalizerStub
}

func (s *terminalizationRuntimeFinalizerStub) FinalizeRouteEvidence(context.Context, FinalizeRouteEvidenceInput) (SealedRouteEvidence, error) {
	return SealedRouteEvidence{}, nil
}

func (s *terminalizationRuntimeFinalizerStub) FinalizeRouteEvidenceFromTerminalization(ctx context.Context, input FinalizeRouteEvidenceFromTerminalizationInput) (int, error) {
	s.mu.Lock()
	s.calls = append(s.calls, input)
	if s.called != nil {
		select {
		case s.called <- struct{}{}:
		default:
		}
	}
	if s.block {
		s.mu.Unlock()
		<-ctx.Done()
		if s.cancelled != nil {
			close(s.cancelled)
		}
		return 0, ctx.Err()
	}
	if remaining := s.failures[input.EventID]; remaining > 0 {
		s.failures[input.EventID] = remaining - 1
		s.mu.Unlock()
		return 0, errors.New("temporary finalizer failure")
	}
	sealed := s.sealed[input.EventID]
	s.mu.Unlock()
	return sealed, nil
}

func (s *terminalizationRuntimeFinalizerStub) Calls() []FinalizeRouteEvidenceFromTerminalizationInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]FinalizeRouteEvidenceFromTerminalizationInput(nil), s.calls...)
}

type terminalizationRuntimeSchedulerStub struct {
	mu       sync.Mutex
	name     string
	interval time.Duration
	callback func()
	canceled string
}

func (s *terminalizationRuntimeSchedulerStub) ScheduleRecurring(name string, interval time.Duration, callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name, s.interval, s.callback = name, interval, callback
}

func (s *terminalizationRuntimeSchedulerStub) Cancel(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled = name
}

func terminalizationEvent() RouteEvidenceTerminalizationEvent {
	return RouteEvidenceTerminalizationEvent{ID: uuid.New(), RunID: uuid.New(), ControlEpoch: 2}
}

func TestRouteEvidenceTerminalizationRuntimeProcessesPendingEvents(t *testing.T) {
	first := terminalizationEvent()
	second := terminalizationEvent()
	repo := &terminalizationRuntimeRepositoryStub{events: []RouteEvidenceTerminalizationEvent{first, second}}
	finalizer := &terminalizationRuntimeFinalizerStub{sealed: map[uuid.UUID]int{first.ID: 2, second.ID: 3}}
	runtime := NewRouteEvidenceTerminalizationRuntime(repo, finalizer, time.Minute, 100)

	result, err := runtime.ProcessPending(context.Background())

	require.NoError(t, err)
	require.Equal(t, RouteEvidenceTerminalizationResult{Selected: 2, Sealed: 5, Processed: 2}, result)
	require.Equal(t, []FinalizeRouteEvidenceFromTerminalizationInput{
		{EventID: first.ID, RunID: first.RunID, ControlEpoch: first.ControlEpoch},
		{EventID: second.ID, RunID: second.RunID, ControlEpoch: second.ControlEpoch},
	}, finalizer.Calls())
}

func TestRouteEvidenceTerminalizationRuntimeRetriesEventAfterFinalizerFailure(t *testing.T) {
	event := terminalizationEvent()
	repo := &terminalizationRuntimeRepositoryStub{events: []RouteEvidenceTerminalizationEvent{event}}
	finalizer := &terminalizationRuntimeFinalizerStub{sealed: map[uuid.UUID]int{event.ID: 1}, failures: map[uuid.UUID]int{event.ID: 1}}
	runtime := NewRouteEvidenceTerminalizationRuntime(repo, finalizer, time.Minute, 100)

	first, firstErr := runtime.ProcessPending(context.Background())
	second, secondErr := runtime.ProcessPending(context.Background())

	require.Error(t, firstErr)
	require.Equal(t, RouteEvidenceTerminalizationResult{Selected: 1, Failed: 1}, first)
	require.NoError(t, secondErr)
	require.Equal(t, RouteEvidenceTerminalizationResult{Selected: 1, Sealed: 1, Processed: 1}, second)
	require.Len(t, finalizer.Calls(), 2)
}

func TestRouteEvidenceTerminalizationRuntimeContinuesAfterOneEventFails(t *testing.T) {
	failed := terminalizationEvent()
	succeeded := terminalizationEvent()
	repo := &terminalizationRuntimeRepositoryStub{events: []RouteEvidenceTerminalizationEvent{failed, succeeded}}
	finalizer := &terminalizationRuntimeFinalizerStub{
		sealed:   map[uuid.UUID]int{succeeded.ID: 4},
		failures: map[uuid.UUID]int{failed.ID: 1},
	}
	runtime := NewRouteEvidenceTerminalizationRuntime(repo, finalizer, time.Minute, 100)

	result, err := runtime.ProcessPending(context.Background())

	require.Error(t, err)
	require.Equal(t, RouteEvidenceTerminalizationResult{Selected: 2, Sealed: 4, Processed: 1, Failed: 1}, result)
	require.Equal(t, []FinalizeRouteEvidenceFromTerminalizationInput{
		{EventID: failed.ID, RunID: failed.RunID, ControlEpoch: failed.ControlEpoch},
		{EventID: succeeded.ID, RunID: succeeded.RunID, ControlEpoch: succeeded.ControlEpoch},
	}, finalizer.Calls())
}

func TestRouteEvidenceTerminalizationRuntimeStartsImmediately(t *testing.T) {
	event := terminalizationEvent()
	repo := &terminalizationRuntimeRepositoryStub{events: []RouteEvidenceTerminalizationEvent{event}}
	finalizer := &terminalizationRuntimeFinalizerStub{sealed: map[uuid.UUID]int{event.ID: 1}, called: make(chan struct{}, 1)}
	scheduler := &terminalizationRuntimeSchedulerStub{}
	runtime := NewRouteEvidenceTerminalizationRuntime(repo, finalizer, time.Minute, 100)
	runtime.SetScheduler(scheduler)

	runtime.Start()

	require.Eventually(t, func() bool { return len(finalizer.Calls()) == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, routeEvidenceTerminalizationTimerName, scheduler.name)
	runtime.Stop()
	require.Equal(t, routeEvidenceTerminalizationTimerName, scheduler.canceled)
}

func TestRouteEvidenceTerminalizationRuntimePreventsOverlappingRuns(t *testing.T) {
	event := terminalizationEvent()
	repo := &terminalizationRuntimeRepositoryStub{events: []RouteEvidenceTerminalizationEvent{event}}
	finalizer := &terminalizationRuntimeFinalizerStub{block: true, called: make(chan struct{}, 1), cancelled: make(chan struct{})}
	runtime := NewRouteEvidenceTerminalizationRuntime(repo, finalizer, time.Minute, 100)

	go runtime.runOnce()
	require.Eventually(t, func() bool { return len(finalizer.Calls()) == 1 }, time.Second, 10*time.Millisecond)
	runtime.runOnce()
	require.Equal(t, 1, repo.ListCalls())
	runtime.Stop()
	require.Eventually(t, func() bool {
		select {
		case <-finalizer.cancelled:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestProvideRouteEvidenceTerminalizationRuntimeLeavesDisabledRadarStopped(t *testing.T) {
	scheduler := &terminalizationRuntimeSchedulerStub{}
	repo := &terminalizationRuntimeContractStub{
		terminalizationRuntimeRepositoryStub: &terminalizationRuntimeRepositoryStub{},
		terminalizationRuntimeFinalizerStub:  &terminalizationRuntimeFinalizerStub{},
	}
	runtime := ProvideRouteEvidenceTerminalizationRuntime(repo, scheduler, &config.Config{})

	require.Empty(t, scheduler.name)
	runtime.Stop()
}

func TestProvideRouteEvidenceTerminalizationRuntimeStartsOnlyForEnabledContract(t *testing.T) {
	event := terminalizationEvent()
	scheduler := &terminalizationRuntimeSchedulerStub{}
	repo := &terminalizationRuntimeContractStub{
		terminalizationRuntimeRepositoryStub: &terminalizationRuntimeRepositoryStub{events: []RouteEvidenceTerminalizationEvent{event}},
		terminalizationRuntimeFinalizerStub:  &terminalizationRuntimeFinalizerStub{called: make(chan struct{}, 1)},
	}
	runtime := ProvideRouteEvidenceTerminalizationRuntime(repo, scheduler, &config.Config{Radar: config.RadarConfig{Enabled: true}})

	require.Eventually(t, func() bool { return len(repo.Calls()) == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, routeEvidenceTerminalizationTimerName, scheduler.name)
	runtime.Stop()

	missingFinalizer := &terminalizationRuntimeRepositoryStub{}
	disabled := &terminalizationRuntimeSchedulerStub{}
	ProvideRouteEvidenceTerminalizationRuntime(missingFinalizer, disabled, &config.Config{Radar: config.RadarConfig{Enabled: true}})
	require.Empty(t, disabled.name)
}

func TestRouteEvidenceTerminalizationRuntimeCancelsInFlightProcessingOnStop(t *testing.T) {
	event := terminalizationEvent()
	repo := &terminalizationRuntimeRepositoryStub{events: []RouteEvidenceTerminalizationEvent{event}}
	finalizer := &terminalizationRuntimeFinalizerStub{block: true, called: make(chan struct{}, 1), cancelled: make(chan struct{})}
	scheduler := &terminalizationRuntimeSchedulerStub{}
	runtime := NewRouteEvidenceTerminalizationRuntime(repo, finalizer, time.Minute, 100)
	runtime.SetScheduler(scheduler)

	runtime.Start()
	require.Eventually(t, func() bool { return len(finalizer.Calls()) == 1 }, time.Second, 10*time.Millisecond)
	runtime.Stop()

	select {
	case <-finalizer.cancelled:
	case <-time.After(time.Second):
		t.Fatal("terminalization finalizer did not receive shutdown cancellation")
	}
	require.Equal(t, routeEvidenceTerminalizationTimerName, scheduler.canceled)
}

func TestLogRouteEvidenceTerminalizationUsesInfoForSuccessfulEmptyBatch(t *testing.T) {
	core, observed := observer.New(zap.DebugLevel)

	logRouteEvidenceTerminalization(zap.New(core), RouteEvidenceTerminalizationResult{}, nil)

	entries := observed.All()
	require.Len(t, entries, 1)
	require.Equal(t, zap.InfoLevel, entries[0].Level)
	require.Equal(t, "radar route evidence terminalization completed", entries[0].Message)
	require.Equal(t, map[string]any{
		"selected":  int64(0),
		"sealed":    int64(0),
		"processed": int64(0),
		"failed":    int64(0),
	}, entries[0].ContextMap())
}

func TestLogRouteEvidenceTerminalizationUsesErrorForProcessingFailure(t *testing.T) {
	core, observed := observer.New(zap.DebugLevel)
	processErr := errors.New("list failed")

	logRouteEvidenceTerminalization(zap.New(core), RouteEvidenceTerminalizationResult{Failed: 1}, processErr)

	entries := observed.All()
	require.Len(t, entries, 1)
	require.Equal(t, zap.ErrorLevel, entries[0].Level)
	require.Equal(t, "radar route evidence terminalization failed", entries[0].Message)
	require.Equal(t, "list failed", entries[0].ContextMap()["error"])
}
