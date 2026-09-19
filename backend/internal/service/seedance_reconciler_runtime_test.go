//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type seedanceReconcilerTaskRepoStub struct {
	AsyncVideoBillingTaskRepository

	mu             sync.Mutex
	claimBatches   [][]AsyncVideoBillingTask
	claimCalls     int
	retryCalls     []seedanceSettlementRetryCall
	terminalCalls  []seedanceSettlementTerminalCall
	settledCalls   []AsyncVideoBillingLease
	claimStarted   chan struct{}
	claimStartOnce sync.Once
}

func (s *seedanceReconcilerTaskRepoStub) ClaimDue(_ context.Context, _ string, _ time.Time, _ int, _ time.Duration) ([]AsyncVideoBillingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	if s.claimStarted != nil {
		s.claimStartOnce.Do(func() { close(s.claimStarted) })
	}
	if len(s.claimBatches) == 0 {
		return nil, nil
	}
	batch := append([]AsyncVideoBillingTask(nil), s.claimBatches[0]...)
	s.claimBatches = s.claimBatches[1:]
	return batch, nil
}

func (s *seedanceReconcilerTaskRepoStub) MarkRetry(_ context.Context, lease AsyncVideoBillingLease, nextPollAt time.Time, errorCode string, missingTokenChecks int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryCalls = append(s.retryCalls, seedanceSettlementRetryCall{
		Lease:              lease,
		NextPollAt:         nextPollAt,
		ErrorCode:          errorCode,
		MissingTokenChecks: missingTokenChecks,
	})
	return nil
}

func (s *seedanceReconcilerTaskRepoStub) MarkTerminal(_ context.Context, lease AsyncVideoBillingLease, status, errorCode string, terminalAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminalCalls = append(s.terminalCalls, seedanceSettlementTerminalCall{
		Lease:      lease,
		Status:     status,
		ErrorCode:  errorCode,
		TerminalAt: terminalAt,
	})
	return nil
}

func (s *seedanceReconcilerTaskRepoStub) MarkSettled(_ context.Context, lease AsyncVideoBillingLease, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settledCalls = append(s.settledCalls, lease)
	return nil
}

func (s *seedanceReconcilerTaskRepoStub) ClaimCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimCalls
}

func (s *seedanceReconcilerTaskRepoStub) RetryCalls() []seedanceSettlementRetryCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seedanceSettlementRetryCall(nil), s.retryCalls...)
}

func (s *seedanceReconcilerTaskRepoStub) TerminalCalls() []seedanceSettlementTerminalCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seedanceSettlementTerminalCall(nil), s.terminalCalls...)
}

func (s *seedanceReconcilerTaskRepoStub) SettledCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.settledCalls)
}

type seedanceReconcilerAccountRepoStub struct {
	AccountRepository

	mu      sync.Mutex
	account *Account
	err     error
	calls   []int64
}

func (s *seedanceReconcilerAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, id)
	return s.account, s.err
}

type seedanceReconcilerClientStub struct {
	mu sync.Mutex

	responses  []*SeedanceUpstreamResponse
	errors     []error
	calls      int
	blockFirst bool
	started    chan struct{}
	cancelled  chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
}

func (s *seedanceReconcilerClientStub) CreateSeedanceTask(context.Context, *Account, []byte) (*SeedanceUpstreamResponse, error) {
	return nil, errors.New("unexpected Seedance create")
}

func (s *seedanceReconcilerClientStub) GetSeedanceTask(ctx context.Context, _ *Account, _ string) (*SeedanceUpstreamResponse, error) {
	s.mu.Lock()
	call := s.calls
	s.calls++
	block := s.blockFirst && call == 0
	var response *SeedanceUpstreamResponse
	if call < len(s.responses) {
		response = s.responses[call]
	}
	var err error
	if call < len(s.errors) {
		err = s.errors[call]
	}
	if block && s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	s.mu.Unlock()

	if block {
		<-ctx.Done()
		if s.cancelled != nil {
			s.cancelOnce.Do(func() { close(s.cancelled) })
		}
		return nil, ctx.Err()
	}
	return response, err
}

func (s *seedanceReconcilerClientStub) DeleteSeedanceTask(context.Context, *Account, string) (*SeedanceUpstreamResponse, error) {
	return nil, errors.New("unexpected Seedance delete")
}

func (s *seedanceReconcilerClientStub) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type seedanceReconcilerSchedulerStub struct {
	mu sync.Mutex

	name        string
	interval    time.Duration
	callback    func()
	scheduleCnt int
	cancelCnt   int
}

func (s *seedanceReconcilerSchedulerStub) ScheduleRecurring(name string, interval time.Duration, callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
	s.interval = interval
	s.callback = callback
	s.scheduleCnt++
}

func (s *seedanceReconcilerSchedulerStub) Cancel(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelCnt++
}

func (s *seedanceReconcilerSchedulerStub) Callback() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callback
}

func seedanceReconcilerClaimedTask(now time.Time) AsyncVideoBillingTask {
	task := seedanceSettlementClaimedTask(now)
	task.AttemptCount = 1
	return task
}

func newSeedanceReconcilerTestRuntime(
	tasks *seedanceReconcilerTaskRepoStub,
	client *seedanceReconcilerClientStub,
	options SeedanceReconcilerOptions,
) (*SeedanceReconcilerRuntime, *seedanceSettlementHarness, *seedanceReconcilerAccountRepoStub) {
	settlementHarness := newSeedanceSettlementHarness()
	settlementHarness.service.tasks = tasks
	accounts := &seedanceReconcilerAccountRepoStub{account: settlementHarness.accounts.value}
	runtime := NewSeedanceReconcilerRuntime(tasks, accounts, client, settlementHarness.service, options)
	return runtime, settlementHarness, accounts
}

func TestSeedanceReconcilerTransient404UsesGraceThenDeadLetters(t *testing.T) {
	now := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	fresh := seedanceReconcilerClaimedTask(now)
	fresh.CreatedAt = now.Add(-30 * time.Second)
	persistent := seedanceReconcilerClaimedTask(now)
	persistent.CreatedAt = now.Add(-61 * time.Second)
	persistent.LeaseEpoch = 2
	persistentToken := uuid.New()
	persistent.LeaseToken = &persistentToken
	tasks := &seedanceReconcilerTaskRepoStub{claimBatches: [][]AsyncVideoBillingTask{{fresh}, {persistent}}}
	client := &seedanceReconcilerClientStub{errors: []error{
		&SeedanceUpstreamError{Kind: SeedanceUpstreamErrorNotFound, StatusCode: 404, SafeCode: "http_404"},
		&SeedanceUpstreamError{Kind: SeedanceUpstreamErrorNotFound, StatusCode: 404, SafeCode: "http_404"},
	}}
	runtime, _, _ := newSeedanceReconcilerTestRuntime(tasks, client, SeedanceReconcilerOptions{
		Enabled:        true,
		RequestTimeout: time.Second,
		LeaseDuration:  time.Minute,
		NotFoundGrace:  time.Minute,
		ClaimBatch:     8,
		MaxConcurrency: 1,
	})
	runtime.now = func() time.Time { return now }
	runtime.jitter = func() float64 { return 0 }

	first, firstErr := runtime.ProcessDue(context.Background())
	second, secondErr := runtime.ProcessDue(context.Background())

	require.NoError(t, firstErr)
	require.Equal(t, SeedanceReconcilerRunResult{Selected: 1, Retried: 1}, first)
	require.NoError(t, secondErr)
	require.Equal(t, SeedanceReconcilerRunResult{Selected: 1, DeadLettered: 1}, second)
	retries := tasks.RetryCalls()
	require.Len(t, retries, 1)
	require.Equal(t, "seedance_not_found_eventual", retries[0].ErrorCode)
	require.Equal(t, now.Add(seedanceRetryDelay(fresh.AttemptCount, 0)), retries[0].NextPollAt)
	terminals := tasks.TerminalCalls()
	require.Len(t, terminals, 1)
	require.Equal(t, AsyncVideoBillingStatusDeadLetter, terminals[0].Status)
	require.Equal(t, "seedance_not_found_persistent", terminals[0].ErrorCode)
}

func TestSeedanceReconcilerRetryDelayIsBounded(t *testing.T) {
	require.Equal(t, 5*time.Second, seedanceRetryDelay(1, 0))
	require.Equal(t, 10*time.Second, seedanceRetryDelay(2, 0))
	require.Equal(t, time.Minute, seedanceRetryDelay(5, 0))
	require.Equal(t, 5*time.Minute, seedanceRetryDelay(6, 0))
	require.Equal(t, 4500*time.Millisecond, seedanceRetryDelay(1, -1))
	require.Equal(t, 5500*time.Millisecond, seedanceRetryDelay(1, 1))
}

func TestSeedanceReconcilerPreventsOverlappingRuns(t *testing.T) {
	now := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	tasks := &seedanceReconcilerTaskRepoStub{claimBatches: [][]AsyncVideoBillingTask{{seedanceReconcilerClaimedTask(now)}}}
	client := &seedanceReconcilerClientStub{
		blockFirst: true,
		started:    make(chan struct{}),
		cancelled:  make(chan struct{}),
	}
	runtime, _, _ := newSeedanceReconcilerTestRuntime(tasks, client, SeedanceReconcilerOptions{
		Enabled:        true,
		PollInterval:   time.Minute,
		RequestTimeout: time.Hour,
		LeaseDuration:  2 * time.Hour,
		ClaimBatch:     8,
		MaxConcurrency: 1,
	})
	runtime.now = func() time.Time { return now }
	scheduler := &seedanceReconcilerSchedulerStub{}
	runtime.SetScheduler(scheduler)

	runtime.Start()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("first reconciler request did not start")
	}
	callback := scheduler.Callback()
	require.NotNil(t, callback)
	callback()
	time.Sleep(25 * time.Millisecond)
	require.Equal(t, 1, tasks.ClaimCalls())

	runtime.Stop()
	select {
	case <-client.cancelled:
	case <-time.After(time.Second):
		t.Fatal("in-flight reconciler request was not cancelled")
	}
}

func TestSeedanceReconcilerTwoRuntimesClaimOneTask(t *testing.T) {
	now := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	tasks := &seedanceReconcilerTaskRepoStub{claimBatches: [][]AsyncVideoBillingTask{{seedanceReconcilerClaimedTask(now)}}}
	client := &seedanceReconcilerClientStub{responses: []*SeedanceUpstreamResponse{{State: SeedanceObservedPending}}}
	options := SeedanceReconcilerOptions{
		Enabled:        true,
		RequestTimeout: time.Second,
		LeaseDuration:  time.Minute,
		ClaimBatch:     8,
		MaxConcurrency: 1,
	}
	firstRuntime, _, _ := newSeedanceReconcilerTestRuntime(tasks, client, options)
	secondRuntime, _, _ := newSeedanceReconcilerTestRuntime(tasks, client, options)
	firstRuntime.now = func() time.Time { return now }
	secondRuntime.now = func() time.Time { return now }

	results := make(chan SeedanceReconcilerRunResult, 2)
	errorsSeen := make(chan error, 2)
	var wg sync.WaitGroup
	for _, runtime := range []*SeedanceReconcilerRuntime{firstRuntime, secondRuntime} {
		wg.Add(1)
		go func(runtime *SeedanceReconcilerRuntime) {
			defer wg.Done()
			result, err := runtime.ProcessDue(context.Background())
			results <- result
			errorsSeen <- err
		}(runtime)
	}
	wg.Wait()
	close(results)
	close(errorsSeen)

	selected := 0
	retried := 0
	for result := range results {
		selected += result.Selected
		retried += result.Retried
	}
	for err := range errorsSeen {
		require.NoError(t, err)
	}
	require.Equal(t, 1, selected)
	require.Equal(t, 1, retried)
	require.Equal(t, 1, client.Calls())
}

func TestSeedanceReconcilerStopCancelsRequestAndRestartRecovers(t *testing.T) {
	firstNow := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	secondNow := firstNow.Add(2 * time.Minute)
	firstTask := seedanceReconcilerClaimedTask(firstNow)
	secondTask := seedanceReconcilerClaimedTask(secondNow)
	secondTask.ID = firstTask.ID
	secondTask.UpstreamTaskID = firstTask.UpstreamTaskID
	secondTask.TaskKey = firstTask.TaskKey
	secondTask.LeaseEpoch = firstTask.LeaseEpoch + 1
	secondToken := uuid.New()
	secondTask.LeaseToken = &secondToken
	tasks := &seedanceReconcilerTaskRepoStub{claimBatches: [][]AsyncVideoBillingTask{{firstTask}, {secondTask}}}
	client := &seedanceReconcilerClientStub{
		blockFirst: true,
		started:    make(chan struct{}),
		cancelled:  make(chan struct{}),
		responses: []*SeedanceUpstreamResponse{
			nil,
			seedanceSettlementSucceeded(90),
		},
	}
	runtime, settlementHarness, _ := newSeedanceReconcilerTestRuntime(tasks, client, SeedanceReconcilerOptions{
		Enabled:        true,
		PollInterval:   time.Hour,
		RequestTimeout: time.Hour,
		LeaseDuration:  time.Minute,
		ClaimBatch:     8,
		MaxConcurrency: 1,
	})
	settlementHarness.service.recordUsage = func(context.Context, *OpenAIRecordUsageInput) error { return nil }
	currentNow := firstNow
	runtime.now = func() time.Time { return currentNow }
	scheduler := &seedanceReconcilerSchedulerStub{}
	runtime.SetScheduler(scheduler)

	runtime.Start()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("first reconciler request did not start")
	}
	runtime.Stop()
	select {
	case <-client.cancelled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the request")
	}

	currentNow = secondNow
	runtime.Start()
	require.Eventually(t, func() bool { return tasks.SettledCount() == 1 }, time.Second, 10*time.Millisecond)
	runtime.Stop()
	require.Equal(t, 2, client.Calls())
	require.Equal(t, 2, scheduler.scheduleCnt)
	require.Equal(t, 2, scheduler.cancelCnt)
}
