//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type seedanceEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *seedanceEventLog) add(event string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *seedanceEventLog) snapshot() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type seedanceDurableTaskRepoStub struct {
	service.AsyncVideoBillingTaskRepository

	mu            sync.Mutex
	recorder      *httptest.ResponseRecorder
	errors        []error
	creates       []service.CreateAsyncVideoBillingTaskInput
	bodyLengths   []int
	existing      *service.AsyncVideoBillingTask
	owned         *service.AsyncVideoBillingTask
	getOwnedErr   error
	getOwnedCalls []seedanceGetOwnedCall
	claimErr      error
	claimCalls    []seedanceClaimOwnedCall
	retryCalls    []seedanceMarkRetryCall
	terminalCalls []seedanceMarkTerminalCall
	events        *seedanceEventLog
}

type seedanceGetOwnedCall struct {
	provider, upstreamTaskID string
	userID, apiKeyID         int64
}

type seedanceClaimOwnedCall struct {
	taskID, userID, apiKeyID int64
}

type seedanceMarkRetryCall struct {
	lease              service.AsyncVideoBillingLease
	errorCode          string
	missingTokenChecks int
}

type seedanceMarkTerminalCall struct {
	lease     service.AsyncVideoBillingLease
	status    string
	errorCode string
}

func (s *seedanceDurableTaskRepoStub) Create(_ context.Context, input service.CreateAsyncVideoBillingTaskInput) (*service.AsyncVideoBillingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates = append(s.creates, input)
	if s.recorder != nil {
		s.bodyLengths = append(s.bodyLengths, s.recorder.Body.Len())
	}
	if len(s.errors) > 0 {
		err := s.errors[0]
		s.errors = s.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	if s.existing != nil {
		copy := *s.existing
		return &copy, nil
	}
	created := &service.AsyncVideoBillingTask{
		ID:                  1,
		Provider:            input.Provider,
		UpstreamTaskID:      input.UpstreamTaskID,
		TaskKey:             input.TaskKey,
		UserID:              input.UserID,
		APIKeyID:            input.APIKeyID,
		AccountID:           input.AccountID,
		GroupID:             input.GroupID,
		SubscriptionID:      input.SubscriptionID,
		Model:               input.Model,
		BillingModel:        input.BillingModel,
		UpstreamModel:       input.UpstreamModel,
		OriginalModel:       input.OriginalModel,
		QuotaPlatform:       input.QuotaPlatform,
		SubscriptionBilling: input.SubscriptionBilling,
		PricingAt:           input.PricingAt,
		RequestPayloadHash:  input.RequestPayloadHash,
		InboundEndpoint:     input.InboundEndpoint,
		UpstreamEndpoint:    input.UpstreamEndpoint,
		Status:              service.AsyncVideoBillingStatusPending,
		NextPollAt:          input.NextPollAt,
		PollDeadlineAt:      input.PollDeadlineAt,
		CreatedAt:           input.PricingAt,
		UpdatedAt:           input.PricingAt,
	}
	owned := *created
	s.owned = &owned
	return created, nil
}

func (s *seedanceDurableTaskRepoStub) snapshot() ([]service.CreateAsyncVideoBillingTaskInput, []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]service.CreateAsyncVideoBillingTaskInput(nil), s.creates...), append([]int(nil), s.bodyLengths...)
}

func (s *seedanceDurableTaskRepoStub) GetOwned(_ context.Context, provider, upstreamTaskID string, userID, apiKeyID int64) (*service.AsyncVideoBillingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getOwnedCalls = append(s.getOwnedCalls, seedanceGetOwnedCall{
		provider: provider, upstreamTaskID: upstreamTaskID, userID: userID, apiKeyID: apiKeyID,
	})
	if s.getOwnedErr != nil {
		return nil, s.getOwnedErr
	}
	if s.owned == nil {
		return nil, service.ErrAsyncVideoBillingTaskNotFound
	}
	owned := *s.owned
	return &owned, nil
}

func (s *seedanceDurableTaskRepoStub) getOwnedSnapshot() []seedanceGetOwnedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seedanceGetOwnedCall(nil), s.getOwnedCalls...)
}

func (s *seedanceDurableTaskRepoStub) ClaimOwned(_ context.Context, taskID, userID, apiKeyID int64, _ time.Time, _ time.Duration) (*service.AsyncVideoBillingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls = append(s.claimCalls, seedanceClaimOwnedCall{taskID: taskID, userID: userID, apiKeyID: apiKeyID})
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	if s.owned == nil {
		return nil, service.ErrAsyncVideoBillingTaskNotFound
	}
	claimed := *s.owned
	token := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	claimed.LeaseToken = &token
	claimed.LeaseEpoch = max(claimed.LeaseEpoch+1, 1)
	return &claimed, nil
}

func (s *seedanceDurableTaskRepoStub) MarkRetry(_ context.Context, lease service.AsyncVideoBillingLease, _ time.Time, errorCode string, missingTokenChecks int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryCalls = append(s.retryCalls, seedanceMarkRetryCall{lease: lease, errorCode: errorCode, missingTokenChecks: missingTokenChecks})
	return nil
}

func (s *seedanceDurableTaskRepoStub) MarkTerminal(_ context.Context, lease service.AsyncVideoBillingLease, status, errorCode string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminalCalls = append(s.terminalCalls, seedanceMarkTerminalCall{lease: lease, status: status, errorCode: errorCode})
	if status == service.AsyncVideoBillingStatusCancelled {
		s.events.add("cancelled")
	}
	return nil
}

func (s *seedanceDurableTaskRepoStub) transitionSnapshot() ([]seedanceClaimOwnedCall, []seedanceMarkRetryCall, []seedanceMarkTerminalCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seedanceClaimOwnedCall(nil), s.claimCalls...),
		append([]seedanceMarkRetryCall(nil), s.retryCalls...),
		append([]seedanceMarkTerminalCall(nil), s.terminalCalls...)
}

type seedanceSettlementObservation struct {
	owner          service.SeedanceTaskOwner
	upstreamTaskID string
	observed       *service.SeedanceUpstreamResponse
}

type seedanceSettlementObserverStub struct {
	mu           sync.Mutex
	observations []seedanceSettlementObservation
	entered      chan struct{}
	release      chan struct{}
	calls        int32
	bills        int32
	once         sync.Once
	result       service.SeedanceSettlementResult
	processCalls []seedanceSettlementProcessCall
	events       *seedanceEventLog
}

type seedanceSettlementProcessCall struct {
	task     service.AsyncVideoBillingTask
	observed *service.SeedanceUpstreamResponse
}

func (s *seedanceSettlementObserverStub) ObserveOwned(
	_ context.Context,
	owner service.SeedanceTaskOwner,
	upstreamTaskID string,
	observed *service.SeedanceUpstreamResponse,
	_ time.Time,
) service.SeedanceSettlementResult {
	s.mu.Lock()
	s.observations = append(s.observations, seedanceSettlementObservation{
		owner: owner, upstreamTaskID: upstreamTaskID, observed: observed,
	})
	s.mu.Unlock()
	call := atomic.AddInt32(&s.calls, 1)
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		if call == 2 {
			close(s.release)
		}
		select {
		case <-s.release:
		case <-time.After(time.Second):
		}
	}
	s.once.Do(func() { atomic.AddInt32(&s.bills, 1) })
	return s.result
}

func (s *seedanceSettlementObserverStub) ProcessClaimed(
	_ context.Context,
	task service.AsyncVideoBillingTask,
	observed *service.SeedanceUpstreamResponse,
	_ time.Time,
) service.SeedanceSettlementResult {
	s.mu.Lock()
	s.processCalls = append(s.processCalls, seedanceSettlementProcessCall{task: task, observed: observed})
	s.mu.Unlock()
	s.events.add("billing")
	if s.result.Settled {
		s.events.add("settled")
	}
	return s.result
}

func (s *seedanceSettlementObserverStub) snapshot() []seedanceSettlementObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seedanceSettlementObservation(nil), s.observations...)
}

func (s *seedanceSettlementObserverStub) processSnapshot() []seedanceSettlementProcessCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seedanceSettlementProcessCall(nil), s.processCalls...)
}

func newSeedanceDurableCreateRequest(
	t *testing.T,
	repo *seedanceDurableTaskRepoStub,
) (*OpenAIGatewayHandler, *gin.Context, *httptest.ResponseRecorder, *grokMediaSlotUpstream) {
	t.Helper()
	h, _, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	h.SetSeedanceDurableBilling(repo, &service.SeedanceTaskSettlementService{})

	upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
		require.Equal(t, http.MethodPost, req.Method)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"seedance-upstream"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"task-durable","status":"queued"}`)),
		}, nil
	}

	c, recorder := grokMediaSlotContext(context.Background(), true)
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.Group.Platform = service.PlatformOpenAI
	body := `{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v3/contents/generations/tasks", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	repo.recorder = recorder
	return h, c, recorder, upstream
}

func TestSeedanceCreatePersistsBeforeWritingSuccess(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{}
	h, c, recorder, upstream := newSeedanceDurableCreateRequest(t, repo)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"id":"task-durable","status":"queued"}`, recorder.Body.String())
	require.Equal(t, "seedance-upstream", recorder.Header().Get("X-Request-Id"))
	creates, bodyLengths := repo.snapshot()
	require.Len(t, creates, 1)
	require.Equal(t, []int{0}, bodyLengths)
	require.Equal(t, "task-durable", creates[0].UpstreamTaskID)
	require.Equal(t, "seedance:task-durable", creates[0].TaskKey)
	require.Equal(t, 1, upstream.calls)
}

func TestSeedanceCreatePersistenceFailureDoesNotReturnUpstreamSuccess(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{errors: []error{errors.New("database unavailable"), errors.New("database unavailable")}}
	h, c, recorder, upstream := newSeedanceDurableCreateRequest(t, repo)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "task-durable")
	creates, bodyLengths := repo.snapshot()
	require.Len(t, creates, 2)
	require.Equal(t, []int{0, 0}, bodyLengths)
	require.Equal(t, 1, upstream.calls)
}

func TestSeedanceCreatePersistenceFailureLogsExcludeSensitivePayloads(t *testing.T) {
	secrets := []string{
		"sk-secret-value",
		"Bearer abc",
		"private prompt",
		"https://cdn.example/private.mp4",
	}
	repositoryErr := errors.New(strings.Join(secrets, " "))
	repo := &seedanceDurableTaskRepoStub{errors: []error{repositoryErr, repositoryErr}}
	h, c, recorder, _ := newSeedanceDurableCreateRequest(t, repo)
	core, observed := observer.New(zap.DebugLevel)
	body := `{"model":"doubao-seedance","content":[{"type":"text","text":"private prompt"}],"url":"https://cdn.example/private.mp4"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v3/contents/generations/tasks", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer abc")
	request = request.WithContext(logger.IntoContext(request.Context(), zap.New(core)))
	c.Request = request

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	entries := observed.FilterMessage("seedance_task_persistence_failed").All()
	require.Len(t, entries, 1)
	require.Equal(t, zap.ErrorLevel, entries[0].Level)
	fields := entries[0].ContextMap()
	require.Equal(t, service.AsyncVideoBillingProviderSeedance, fields["provider"])
	require.Equal(t, "seedance:task-durable", fields["task_id"])
	require.Equal(t, "persistence_failed", fields["status"])
	require.Equal(t, "seedance_task_persistence_failed", fields["error_code"])
	require.Equal(t, int64(2), fields["attempt_count"])
	require.Contains(t, fields, "elapsed_ms")
	text := fmt.Sprint(entries[0].Message, " ", fields)
	for _, secret := range secrets {
		require.NotContains(t, text, secret)
	}

	t.Run("cancelled retry records the actual attempt count", func(t *testing.T) {
		repo := &seedanceDurableTaskRepoStub{errors: []error{repositoryErr}}
		h, c, _, _ := newSeedanceDurableCreateRequest(t, repo)
		core, observed := observer.New(zap.DebugLevel)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := h.persistSeedanceCreate(
			ctx,
			c,
			zap.New(core),
			&service.Account{ID: 7, Platform: service.PlatformOpenAI},
			&service.APIKey{ID: 8},
			middleware.AuthSubject{UserID: 9},
			nil,
			time.Now(),
			"doubao-seedance",
			[]byte(`{"model":"doubao-seedance","content":[{"type":"text","text":"private prompt"}]}`),
			&service.OpenAIForwardResult{ResponseID: "seedance:task-durable"},
		)

		require.ErrorIs(t, err, errSeedanceTaskPersistence)
		creates, _ := repo.snapshot()
		require.Len(t, creates, 1)
		entries := observed.FilterMessage("seedance_task_persistence_failed").All()
		require.Len(t, entries, 1)
		require.Equal(t, int64(1), entries[0].ContextMap()["attempt_count"])
	})
}

func TestSeedanceCreateDuplicatePersistenceReturnsSuccess(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{existing: &service.AsyncVideoBillingTask{ID: 9, TaskKey: "seedance:task-durable"}}
	h, c, recorder, upstream := newSeedanceDurableCreateRequest(t, repo)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"id":"task-durable","status":"queued"}`, recorder.Body.String())
	creates, bodyLengths := repo.snapshot()
	require.Len(t, creates, 1)
	require.Equal(t, []int{0}, bodyLengths)
	require.Equal(t, 1, upstream.calls)
}

func TestSeedanceCreateWithoutDurableDependenciesFailsClosed(t *testing.T) {
	h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	c, recorder := grokMediaSlotContext(context.Background(), true)
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.Group.Platform = service.PlatformOpenAI
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/v3/contents/generations/tasks",
		strings.NewReader(`{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.Zero(t, upstream.calls)
	slots.assertReleased(t)
}

func TestSetSeedanceDurableBillingNilSettlementKeepsDependencyAbsent(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	h.SetSeedanceDurableBilling(&seedanceDurableTaskRepoStub{}, nil)

	require.True(t, h.seedanceSettlement == nil)
}

func newSeedanceDurableGetRequest(
	t *testing.T,
	repo *seedanceDurableTaskRepoStub,
	settlement *seedanceSettlementObserverStub,
) (*OpenAIGatewayHandler, *gin.Context, *httptest.ResponseRecorder, *grokMediaSlotBindings, *grokMediaSlotUpstream) {
	t.Helper()
	h, _, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	h.seedanceTasks = repo
	h.seedanceSettlement = settlement
	groupID := int64(24)
	require.NoError(t, h.gatewayService.BindGrokMediaVideoRequestAccount(
		context.Background(), &groupID, "seedance:task", 10, 20, 1,
	))
	upstream.call = func(req *http.Request, accountID int64) (*http.Response, error) {
		require.Equal(t, http.MethodGet, req.Method)
		require.Equal(t, int64(1), accountID)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"task","status":"succeeded","model":"doubao-seedance","usage":{"completion_tokens":20}}`)),
		}, nil
	}
	c, recorder := grokMediaSlotContext(context.Background(), false)
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.Group.Platform = service.PlatformOpenAI
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/task", nil)
	c.Params = gin.Params{{Key: "task_id", Value: "task"}}
	return h, c, recorder, bindings, upstream
}

func TestSeedanceGetAndReconcilerRaceBillsOnce(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: &service.AsyncVideoBillingTask{
		ID: 1, Provider: service.AsyncVideoBillingProviderSeedance, UpstreamTaskID: "task",
		TaskKey: "seedance:task", UserID: 10, APIKeyID: 20, AccountID: 1,
		Status: service.AsyncVideoBillingStatusPending,
	}}
	settlement := &seedanceSettlementObserverStub{
		entered: make(chan struct{}, 2), release: make(chan struct{}),
		result: service.SeedanceSettlementResult{Settled: true},
	}
	h, c, recorder, _, upstream := newSeedanceDurableGetRequest(t, repo, settlement)

	reconcilerDone := make(chan struct{})
	go func() {
		defer close(reconcilerDone)
		settlement.ObserveOwned(
			context.Background(),
			service.SeedanceTaskOwner{UserID: 10, APIKeyID: 20},
			"task",
			&service.SeedanceUpstreamResponse{State: service.SeedanceObservedSucceeded},
			time.Now(),
		)
	}()
	<-settlement.entered

	h.SeedanceTasks(c)
	<-reconcilerDone

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, int32(2), atomic.LoadInt32(&settlement.calls))
	require.Equal(t, int32(1), atomic.LoadInt32(&settlement.bills))
	observations := settlement.snapshot()
	require.Len(t, observations, 2)
	require.Equal(t, service.SeedanceTaskOwner{UserID: 10, APIKeyID: 20}, observations[1].owner)
	require.Equal(t, "task", observations[1].upstreamTaskID)
	require.NotNil(t, observations[1].observed)
	require.Equal(t, service.SeedanceObservedSucceeded, observations[1].observed.State)
	require.Equal(t, 20, observations[1].observed.Result.Usage.OutputTokens)
	require.Equal(t, []seedanceGetOwnedCall{{
		provider: service.AsyncVideoBillingProviderSeedance, upstreamTaskID: "task", userID: 10, apiKeyID: 20,
	}}, repo.getOwnedSnapshot())
}

func TestSeedanceGetSettlementRetryKeepsUpstreamResponse(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: &service.AsyncVideoBillingTask{
		ID: 3, Provider: service.AsyncVideoBillingProviderSeedance, UpstreamTaskID: "task",
		TaskKey: "seedance:task", UserID: 10, APIKeyID: 20, AccountID: 1,
		Status: service.AsyncVideoBillingStatusPending,
	}}
	settlement := &seedanceSettlementObserverStub{result: service.SeedanceSettlementResult{
		Retried: true, ErrorCode: "seedance_usage_record_failed", Err: errors.New("database unavailable"),
	}}
	h, c, recorder, _, upstream := newSeedanceDurableGetRequest(t, repo, settlement)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"id":"task","status":"succeeded","model":"doubao-seedance","usage":{"completion_tokens":20}}`, recorder.Body.String())
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, int32(1), atomic.LoadInt32(&settlement.calls))
}

func TestSeedanceDurableLookupUsesPersistedGroupAfterAPIKeyMoves(t *testing.T) {
	groupID := int64(24)
	repo := &seedanceDurableTaskRepoStub{owned: &service.AsyncVideoBillingTask{
		ID: 4, Provider: service.AsyncVideoBillingProviderSeedance, UpstreamTaskID: "task",
		TaskKey: "seedance:task", UserID: 10, APIKeyID: 20, AccountID: 1,
		GroupID: &groupID, Status: service.AsyncVideoBillingStatusPending,
	}}
	settlement := &seedanceSettlementObserverStub{}
	h, _, _, upstream := newGrokMediaSlotHandlerWithRunMode(t, config.RunModeStandard, false, false, service.PlatformOpenAI)
	h.seedanceTasks = repo
	h.seedanceSettlement = settlement
	upstream.call = func(req *http.Request, accountID int64) (*http.Response, error) {
		require.Equal(t, http.MethodGet, req.Method)
		require.Equal(t, int64(1), accountID)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"task","status":"succeeded","model":"doubao-seedance","usage":{"completion_tokens":20}}`)),
		}, nil
	}
	c, recorder := grokMediaSlotContext(context.Background(), false)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/task", nil)
	c.Params = gin.Params{{Key: "task_id", Value: "task"}}
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.Group.Platform = service.PlatformOpenAI
	movedGroupID := int64(99)
	key.GroupID = &movedGroupID
	key.Group = &service.Group{ID: movedGroupID, Platform: service.PlatformOpenAI, AllowImageGeneration: true}

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, upstream.calls)
}

func TestSeedanceCreateUsesConfiguredPollDeadline(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{}
	h, c, _, _ := newSeedanceDurableCreateRequest(t, repo)
	h.cfg.Gateway.SeedanceReconciler.PollDeadlineHours = 6
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	require.True(t, ok)
	requestStart := time.Date(2026, time.September, 20, 1, 2, 3, 0, time.UTC)

	err := h.persistSeedanceCreate(
		context.Background(),
		c,
		zap.NewNop(),
		&service.Account{ID: 1, Platform: service.PlatformOpenAI},
		key,
		subject,
		nil,
		requestStart,
		"doubao-seedance",
		[]byte(`{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`),
		&service.OpenAIForwardResult{ResponseID: "seedance:task-durable"},
	)

	require.NoError(t, err)
	creates, _ := repo.snapshot()
	require.Len(t, creates, 1)
	require.Equal(t, requestStart.Add(6*time.Hour), creates[0].PollDeadlineAt)
}

func TestSeedanceGetOwnerMismatchDoesNotCallUpstream(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: &service.AsyncVideoBillingTask{
		ID: 2, Provider: service.AsyncVideoBillingProviderSeedance, UpstreamTaskID: "task",
		TaskKey: "seedance:task", UserID: 11, APIKeyID: 20, AccountID: 1,
		Status: service.AsyncVideoBillingStatusPending,
	}}
	settlement := &seedanceSettlementObserverStub{}
	h, c, recorder, _, upstream := newSeedanceDurableGetRequest(t, repo, settlement)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
	require.Zero(t, upstream.calls)
	require.Zero(t, atomic.LoadInt32(&settlement.calls))
}

func TestSeedanceGetLegacyRedisPendingStillSettles(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{getOwnedErr: service.ErrAsyncVideoBillingTaskNotFound}
	settlement := &seedanceSettlementObserverStub{}
	h, c, recorder, bindings, upstream := newSeedanceDurableGetRequest(t, repo, settlement)
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(
		context.Background(),
		"seedance:task",
		10,
		20,
		service.GrokVideoPendingBilling{Model: "doubao-seedance", BillingModel: "doubao-seedance"},
	))

	h.SeedanceTasks(c)
	second, secondRecorder := grokMediaSlotContext(context.Background(), false)
	secondKey, ok := middleware.GetAPIKeyFromContext(second)
	require.True(t, ok)
	secondKey.Group.Platform = service.PlatformOpenAI
	second.Request = httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/task", nil)
	second.Params = gin.Params{{Key: "task_id", Value: "task"}}
	h.SeedanceTasks(second)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, http.StatusOK, secondRecorder.Code, secondRecorder.Body.String())
	require.Equal(t, 2, upstream.calls)
	require.Zero(t, atomic.LoadInt32(&settlement.calls))
	require.Len(t, bindings.billed, 1)
}

func newSeedanceDurableDeleteRequest(
	t *testing.T,
	repo *seedanceDurableTaskRepoStub,
	settlement *seedanceSettlementObserverStub,
	statusCode int,
	statusBody string,
) (*OpenAIGatewayHandler, *gin.Context, *httptest.ResponseRecorder, *grokMediaSlotUpstream, *seedanceEventLog) {
	t.Helper()
	events := &seedanceEventLog{}
	repo.events = events
	settlement.events = events
	h, _, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	h.seedanceTasks = repo
	h.seedanceSettlement = settlement
	upstream.call = func(req *http.Request, accountID int64) (*http.Response, error) {
		require.Equal(t, int64(2), accountID)
		switch req.Method {
		case http.MethodGet:
			events.add("status")
			header := http.Header{"Content-Type": []string{"application/json"}}
			if statusCode >= http.StatusInternalServerError {
				header.Set("Retry-After", "2")
			}
			return &http.Response{
				StatusCode: statusCode,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(statusBody)),
			}, nil
		case http.MethodDelete:
			events.add("delete")
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
					"X-Request-Id": []string{"seedance-delete"},
				},
				Body: io.NopCloser(strings.NewReader("")),
			}, nil
		default:
			require.FailNow(t, "unexpected Seedance method", req.Method)
			return nil, nil
		}
	}
	c, recorder := grokMediaSlotContext(context.Background(), false)
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.Group.Platform = service.PlatformOpenAI
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v3/contents/generations/tasks/task", nil)
	c.Params = gin.Params{{Key: "task_id", Value: "task"}}
	return h, c, recorder, upstream, events
}

func pendingSeedanceDeleteTask() *service.AsyncVideoBillingTask {
	return &service.AsyncVideoBillingTask{
		ID: 41, Provider: service.AsyncVideoBillingProviderSeedance, UpstreamTaskID: "task",
		TaskKey: "seedance:task", UserID: 10, APIKeyID: 20, AccountID: 2,
		Model: "doubao-seedance", BillingModel: "doubao-seedance",
		Status: service.AsyncVideoBillingStatusPending, CreatedAt: time.Now().Add(-time.Minute),
		PollDeadlineAt: time.Now().Add(time.Hour),
	}
}

func TestSeedanceDeleteSucceededTaskSettlesBeforeDelete(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: pendingSeedanceDeleteTask()}
	settlement := &seedanceSettlementObserverStub{result: service.SeedanceSettlementResult{Settled: true}}
	h, c, recorder, upstream, events := newSeedanceDurableDeleteRequest(
		t,
		repo,
		settlement,
		http.StatusOK,
		`{"id":"task","status":"succeeded","model":"doubao-seedance","usage":{"completion_tokens":20}}`,
	)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
	require.Equal(t, "seedance-delete", recorder.Header().Get("X-Request-Id"))
	require.Equal(t, []string{"status", "billing", "settled", "delete"}, events.snapshot())
	require.Equal(t, 2, upstream.calls)
	processCalls := settlement.processSnapshot()
	require.Len(t, processCalls, 1)
	require.Equal(t, service.SeedanceObservedSucceeded, processCalls[0].observed.State)
	require.Equal(t, 20, processCalls[0].observed.Result.Usage.OutputTokens)
	require.True(t, service.AsyncVideoBillingLease{
		TaskID: processCalls[0].task.ID, Token: *processCalls[0].task.LeaseToken, Epoch: processCalls[0].task.LeaseEpoch,
	}.Valid())
}

func TestSeedanceDeleteStatusFailureDoesNotDelete(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: pendingSeedanceDeleteTask()}
	settlement := &seedanceSettlementObserverStub{}
	h, c, recorder, upstream, events := newSeedanceDurableDeleteRequest(
		t,
		repo,
		settlement,
		http.StatusServiceUnavailable,
		`{"error":{"code":"temporarily_unavailable"}}`,
	)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.Equal(t, "2", recorder.Header().Get("Retry-After"))
	require.Equal(t, []string{"status"}, events.snapshot())
	require.Equal(t, 1, upstream.calls)
	_, retries, terminals := repo.transitionSnapshot()
	require.Len(t, retries, 1)
	require.Equal(t, "temporarily_unavailable", retries[0].errorCode)
	require.Empty(t, terminals)
	require.Empty(t, settlement.processSnapshot())
}

func TestSeedanceDeleteSuccessfulStatusWithProtocolErrorReturnsRetryableFailure(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: pendingSeedanceDeleteTask()}
	settlement := &seedanceSettlementObserverStub{}
	h, c, recorder, upstream, events := newSeedanceDurableDeleteRequest(
		t,
		repo,
		settlement,
		http.StatusOK,
		`{"id":"task","status":"processing"}`,
	)
	originalCall := upstream.call
	upstream.call = func(req *http.Request, accountID int64) (*http.Response, error) {
		if req.Method != http.MethodDelete {
			return originalCall(req, accountID)
		}
		require.Equal(t, int64(2), accountID)
		events.add("delete")
		h.cfg.Gateway.UpstreamResponseReadMaxBytes = 1
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader("xx")),
		}, nil
	}

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.Equal(t, []string{"status", "delete"}, events.snapshot())
	require.Equal(t, 2, upstream.calls)
	_, retries, terminals := repo.transitionSnapshot()
	require.Len(t, retries, 1)
	require.Equal(t, "response_too_large", retries[0].errorCode)
	require.Empty(t, terminals)
	require.Empty(t, settlement.processSnapshot())
}

func TestSeedanceDeletePendingTaskMarksCancelledAfterDelete(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: pendingSeedanceDeleteTask()}
	settlement := &seedanceSettlementObserverStub{}
	h, c, recorder, upstream, events := newSeedanceDurableDeleteRequest(
		t,
		repo,
		settlement,
		http.StatusOK,
		`{"id":"task","status":"processing"}`,
	)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
	require.Equal(t, []string{"status", "delete", "cancelled"}, events.snapshot())
	require.Equal(t, 2, upstream.calls)
	_, retries, terminals := repo.transitionSnapshot()
	require.Empty(t, retries)
	require.Len(t, terminals, 1)
	require.Equal(t, service.AsyncVideoBillingStatusCancelled, terminals[0].status)
	require.True(t, terminals[0].lease.Valid())
	require.Empty(t, settlement.processSnapshot())
}

func TestSeedanceDeleteOwnerMismatchIsInvisible(t *testing.T) {
	task := pendingSeedanceDeleteTask()
	task.UserID = 99
	repo := &seedanceDurableTaskRepoStub{owned: task}
	settlement := &seedanceSettlementObserverStub{}
	h, c, recorder, upstream, events := newSeedanceDurableDeleteRequest(
		t,
		repo,
		settlement,
		http.StatusOK,
		`{"id":"task","status":"processing"}`,
	)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
	require.Empty(t, events.snapshot())
	require.Zero(t, upstream.calls)
	claims, retries, terminals := repo.transitionSnapshot()
	require.Empty(t, claims)
	require.Empty(t, retries)
	require.Empty(t, terminals)
	require.Empty(t, settlement.processSnapshot())
}
